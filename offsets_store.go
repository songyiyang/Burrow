/* Copyright 2015 LinkedIn Corp. Licensed under the Apache License, Version
 * 2.0 (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 */

package main

import (
	"container/ring"
	"encoding/json"
	"fmt"
	log "github.com/cihub/seelog"
	"regexp"
	"sync"
	"time"
)

type PartitionOffset struct {
	Cluster             string
	Topic               string
	Partition           int32
	Offset              int64
	Timestamp           int64
	Group               string
	TopicPartitionCount int
}

type BrokerOffset struct {
	Offset    int64
	Timestamp int64
}

type ConsumerOffset struct {
	Offset     int64 `json:"offset"`
	Timestamp  int64 `json:"timestamp"`
	Lag        int64 `json:"lag"`
	artificial bool
}

type ClusterOffsets struct {
	broker       map[string][]*BrokerOffset
	consumer     map[string]map[string][]*ring.Ring
	brokerLock   *sync.RWMutex
	consumerLock *sync.RWMutex
}
type OffsetStorage struct {
	app            *ApplicationContext
	quit           chan struct{}
	offsetChannel  chan *PartitionOffset
	requestChannel chan interface{}
	offsets        map[string]*ClusterOffsets
	groupBlacklist *regexp.Regexp
	topicBlacklist *regexp.Regexp
}

type StatusConstant int

const (
	StatusNotFound StatusConstant = 0
	StatusOK       StatusConstant = 1
	StatusWarning  StatusConstant = 2
	StatusError    StatusConstant = 3
	StatusStop     StatusConstant = 4
	StatusStall    StatusConstant = 5
	StatusRewind   StatusConstant = 6
)

var StatusStrings = [...]string{"NOTFOUND", "OK", "WARN", "ERR", "STOP", "STALL", "REWIND"}

func (c StatusConstant) String() string {
	if (c >= 0) && (c < StatusConstant(len(StatusStrings))) {
		return StatusStrings[c]
	} else {
		return "UNKNOWN"
	}
}
func (c StatusConstant) MarshalText() ([]byte, error) {
	return []byte(c.String()), nil
}
func (c StatusConstant) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.String())
}

type PartitionStatus struct {
	Topic     string         `json:"topic"`
	Partition int32          `json:"partition"`
	Status    StatusConstant `json:"status"`
	Start     ConsumerOffset `json:"start"`
	End       ConsumerOffset `json:"end"`
}

type ConsumerGroupStatus struct {
	Cluster         string             `json:"cluster"`
	Group           string             `json:"group"`
	Status          StatusConstant     `json:"status"`
	Complete        bool               `json:"complete"`
	Partitions      []*PartitionStatus `json:"partitions"`
	TotalPartitions int                `json:"partition_count"`
	Maxlag          *PartitionStatus   `json:"maxlag"`
	TotalLag        uint64             `json:"totallag"`
}

type ResponseTopicList struct {
	TopicList []string
	Error     bool
}
type ResponseOffsets struct {
	OffsetList []int64
	ErrorGroup bool
	ErrorTopic bool
}
type RequestClusterList struct {
	Result chan []string
}
type RequestConsumerList struct {
	Result  chan []string
	Cluster string
}
type RequestTopicList struct {
	Result  chan *ResponseTopicList
	Cluster string
	Group   string
}
type RequestOffsets struct {
	Result  chan *ResponseOffsets
	Cluster string
	Topic   string
	Group   string
}
type RequestConsumerStatus struct {
	Result  chan *ConsumerGroupStatus
	Cluster string
	Group   string
	Showall bool
}
type RequestConsumerDrop struct {
	Result  chan StatusConstant
	Cluster string
	Group   string
}

func NewOffsetStorage(app *ApplicationContext) (*OffsetStorage, error) {
	storage := &OffsetStorage{
		app:            app,
		quit:           make(chan struct{}),
		offsetChannel:  make(chan *PartitionOffset, 10000),
		requestChannel: make(chan interface{}),
		offsets:        make(map[string]*ClusterOffsets),
	}

	if app.Config.General.GroupBlacklist != "" {
		re, err := regexp.Compile(app.Config.General.GroupBlacklist)
		if err != nil {
			return nil, err
		}
		storage.groupBlacklist = re
	}

	if app.Config.General.TopicBlacklist != "" {
		re, err := regexp.Compile(app.Config.General.TopicBlacklist)
		if err != nil {
			return nil, err
		}
		storage.topicBlacklist = re
	}

	for cluster, _ := range app.Config.Kafka {
		storage.offsets[cluster] = &ClusterOffsets{
			broker:       make(map[string][]*BrokerOffset),
			consumer:     make(map[string]map[string][]*ring.Ring),
			brokerLock:   &sync.RWMutex{},
			consumerLock: &sync.RWMutex{},
		}
	}

	go func() {
		for {
			select {
			case o := <-storage.offsetChannel:
				if o.Group == "" {
					go storage.addBrokerOffset(o)
				} else {
					go storage.addConsumerOffset(o)
				}
			case r := <-storage.requestChannel:
				switch r.(type) {
				case *RequestConsumerList:
					request, _ := r.(*RequestConsumerList)
					go storage.requestConsumerList(request)
				case *RequestTopicList:
					request, _ := r.(*RequestTopicList)
					go storage.requestTopicList(request)
				case *RequestOffsets:
					request, _ := r.(*RequestOffsets)
					go storage.requestOffsets(request)
				case *RequestConsumerStatus:
					request, _ := r.(*RequestConsumerStatus)
					go storage.evaluateGroup(request.Cluster, request.Group, request.Result, request.Showall)
				case *RequestConsumerDrop:
					request, _ := r.(*RequestConsumerDrop)
					go storage.dropGroup(request.Cluster, request.Group, request.Result)
				default:
					// Silently drop unknown requests
				}
			case <-storage.quit:
				return
			}
		}
	}()

	return storage, nil
}

func (storage *OffsetStorage) addBrokerOffset(offset *PartitionOffset) {
	clusterMap, ok := storage.offsets[offset.Cluster]
	if !ok {
		// Ignore offsets for clusters that we don't know about - should never happen anyways
		return
	}

	clusterMap.brokerLock.Lock()
	topicList, ok := clusterMap.broker[offset.Topic]
	if !ok {
		clusterMap.broker[offset.Topic] = make([]*BrokerOffset, offset.TopicPartitionCount)
		topicList = clusterMap.broker[offset.Topic]
	}
	if offset.TopicPartitionCount >= len(topicList) {
		// The partition count has increased. Append enough extra partitions to our slice
		for i := len(topicList); i < offset.TopicPartitionCount; i++ {
			topicList = append(topicList, nil)
		}
	}

	partitionEntry := topicList[offset.Partition]
	if partitionEntry == nil {
		topicList[offset.Partition] = &BrokerOffset{
			Offset:    offset.Offset,
			Timestamp: offset.Timestamp,
		}
		partitionEntry = topicList[offset.Partition]
	} else {
		partitionEntry.Offset = offset.Offset
		partitionEntry.Timestamp = offset.Timestamp
	}

	clusterMap.brokerLock.Unlock()
}

func (storage *OffsetStorage) addConsumerOffset(offset *PartitionOffset) {
	// Ignore offsets for clusters that we don't know about - should never happen anyways
	clusterOffsets, ok := storage.offsets[offset.Cluster]
	if !ok {
		return
	}

	// Ignore groups that match our blacklist
	if (storage.groupBlacklist != nil) && storage.groupBlacklist.MatchString(offset.Group) || (storage.topicBlacklist != nil) && storage.topicBlacklist.MatchString(offset.Topic) {
		log.Debugf("Dropped offset (blacklist): cluster=%s topic=%s partition=%v group=%s timestamp=%v offset=%v",
			offset.Cluster, offset.Topic, offset.Partition, offset.Group, offset.Timestamp, offset.Offset)
		return
	}

	// Get broker partition count and offset for this topic and partition first
	clusterOffsets.brokerLock.RLock()
	topicPartitionList, ok := clusterOffsets.broker[offset.Topic]
	if !ok {
		// We don't know about this topic from the brokers yet - skip consumer offsets for now
		clusterOffsets.brokerLock.RUnlock()
		log.Debugf("Dropped offset (no topic): cluster=%s topic=%s partition=%v group=%s timestamp=%v offset=%v",
			offset.Cluster, offset.Topic, offset.Partition, offset.Group, offset.Timestamp, offset.Offset)
		return
	}
	if offset.Partition < 0 {
		// This should never happen, but if it does, log an warning with the offset information for review
		log.Warnf("Got a negative partition ID: cluster=%s topic=%s partition=%v group=%s timestamp=%v offset=%v",
			offset.Cluster, offset.Topic, offset.Partition, offset.Group, offset.Timestamp, offset.Offset)
		clusterOffsets.brokerLock.RUnlock()
		return
	}
	if offset.Partition >= int32(len(topicPartitionList)) {
		// We know about the topic, but partitions have been expanded and we haven't seen that from the broker yet
		clusterOffsets.brokerLock.RUnlock()
		log.Debugf("Dropped offset (expanded): cluster=%s topic=%s partition=%v group=%s timestamp=%v offset=%v",
			offset.Cluster, offset.Topic, offset.Partition, offset.Group, offset.Timestamp, offset.Offset)
		return
	}
	if topicPartitionList[offset.Partition] == nil {
		// We know about the topic and partition, but we haven't actually gotten the broker offset yet
		clusterOffsets.brokerLock.RUnlock()
		log.Debugf("Dropped offset (broker offset): cluster=%s topic=%s partition=%v group=%s timestamp=%v offset=%v",
			offset.Cluster, offset.Topic, offset.Partition, offset.Group, offset.Timestamp, offset.Offset)
		return
	}
	brokerOffset := topicPartitionList[offset.Partition].Offset
	partitionCount := len(topicPartitionList)
	clusterOffsets.brokerLock.RUnlock()

	clusterOffsets.consumerLock.Lock()
	consumerMap, ok := clusterOffsets.consumer[offset.Group]
	if !ok {
		clusterOffsets.consumer[offset.Group] = make(map[string][]*ring.Ring)
		consumerMap = clusterOffsets.consumer[offset.Group]
	}
	consumerTopicMap, ok := consumerMap[offset.Topic]
	if !ok {
		consumerMap[offset.Topic] = make([]*ring.Ring, partitionCount)
		consumerTopicMap = consumerMap[offset.Topic]
	}
	if int(offset.Partition) >= len(consumerTopicMap) {
		// The partition count must have increased. Append enough extra partitions to our slice
		for i := len(consumerTopicMap); i < partitionCount; i++ {
			consumerTopicMap = append(consumerTopicMap, nil)
		}
	}

	consumerPartitionRing := consumerTopicMap[offset.Partition]
	if consumerPartitionRing == nil {
		consumerTopicMap[offset.Partition] = ring.New(storage.app.Config.Lagcheck.Intervals)
		consumerPartitionRing = consumerTopicMap[offset.Partition]
	} else {
		lastOffset := consumerPartitionRing.Prev().Value.(*ConsumerOffset)
		timestampDifference := offset.Timestamp - lastOffset.Timestamp

		// Prevent old offset commits, but only if the offsets don't advance (because of artifical commits below)
		if (timestampDifference <= 0) && (offset.Offset <= lastOffset.Offset) {
			clusterOffsets.consumerLock.Unlock()
			log.Debugf("Dropped offset (noadvance): cluster=%s topic=%s partition=%v group=%s timestamp=%v offset=%v tsdiff=%v lag=%v",
				offset.Cluster, offset.Topic, offset.Partition, offset.Group, offset.Timestamp, offset.Offset,
				timestampDifference, brokerOffset-offset.Offset)
			return
		}

		// Prevent new commits that are too fast (less than the min-distance config) if the last offset was not artificial
		if (!lastOffset.artificial) && (timestampDifference >= 0) && (timestampDifference < (storage.app.Config.Lagcheck.MinDistance * 1000)) {
			clusterOffsets.consumerLock.Unlock()
			log.Debugf("Dropped offset (mindistance): cluster=%s topic=%s partition=%v group=%s timestamp=%v offset=%v tsdiff=%v lag=%v",
				offset.Cluster, offset.Topic, offset.Partition, offset.Group, offset.Timestamp, offset.Offset,
				timestampDifference, brokerOffset-offset.Offset)
			return
		}
	}

	// Calculate the lag against the brokerOffset
	partitionLag := brokerOffset - offset.Offset
	if partitionLag < 0 {
		// Little bit of a hack - because we only get broker offsets periodically, it's possible the consumer offset could be ahead of where we think the broker
		// is. In this case, just mark it as zero lag.
		partitionLag = 0
	}

	// Update or create the ring value at the current pointer
	if consumerPartitionRing.Value == nil {
		consumerPartitionRing.Value = &ConsumerOffset{
			Offset:     offset.Offset,
			Timestamp:  offset.Timestamp,
			Lag:        partitionLag,
			artificial: false,
		}
	} else {
		ringval, _ := consumerPartitionRing.Value.(*ConsumerOffset)
		ringval.Offset = offset.Offset
		ringval.Timestamp = offset.Timestamp
		ringval.Lag = partitionLag
		ringval.artificial = false
	}

	log.Tracef("Commit offset: cluster=%s topic=%s partition=%v group=%s timestamp=%v offset=%v lag=%v",
		offset.Cluster, offset.Topic, offset.Partition, offset.Group, offset.Timestamp, offset.Offset,
		partitionLag)

	// Advance the ring pointer
	consumerTopicMap[offset.Partition] = consumerTopicMap[offset.Partition].Next()
	clusterOffsets.consumerLock.Unlock()
}

func (storage *OffsetStorage) Stop() {
	close(storage.quit)
}

func (storage *OffsetStorage) dropGroup(cluster string, group string, resultChannel chan StatusConstant) {
	storage.offsets[cluster].consumerLock.Lock()

	if _, ok := storage.offsets[cluster].consumer[group]; ok {
		log.Infof("Removing group %s from cluster %s by request", group, cluster)
		delete(storage.offsets[cluster].consumer, group)
		resultChannel <- StatusOK
	} else {
		resultChannel <- StatusNotFound
	}

	storage.offsets[cluster].consumerLock.Unlock()
}

// Evaluate a consumer group based on specific rules about lag
// Rule 1:  If over the stored period, the lag is ever zero for the partition, the period is OK
// Rule 2:  If the consumer offset does not change, and the lag is non-zero, it's an error (partition is stalled)
// Rule 3:  If the consumer offsets are moving, but the lag is consistently increasing, it's a warning (consumer is slow)
// Rule 4:  If the difference between now and the last offset timestamp is greater than the difference between the last and first offset timestamps, the
//          consumer has stopped committing offsets for that partition (error), unless
// Rule 5:  If the lag is -1, this is a special value that means there is no broker offset yet. Consider it good (will get caught in the next refresh of topics)
// Rule 6:  If the consumer offset decreases from one interval to the next the partition is marked as a rewind (error)
func (storage *OffsetStorage) evaluateGroup(cluster string, group string, resultChannel chan *ConsumerGroupStatus, showall bool) {
	status := &ConsumerGroupStatus{
		Cluster:    cluster,
		Group:      group,
		Status:     StatusNotFound,
		Complete:   true,
		Partitions: make([]*PartitionStatus, 0),
		Maxlag:     nil,
		TotalLag:   0,
	}

	// Make sure the cluster exists
	clusterMap, ok := storage.offsets[cluster]
	if !ok {
		resultChannel <- status
		return
	}

	// Make sure the group even exists
	clusterMap.consumerLock.Lock()
	consumerMap, ok := clusterMap.consumer[group]
	if !ok {
		clusterMap.consumerLock.Unlock()
		resultChannel <- status
		return
	}

	// Scan the offsets table once and store all the offsets for the group locally
	status.Status = StatusOK
	offsetList := make(map[string][][]ConsumerOffset, len(consumerMap))
	var youngestOffset int64
	for topic, partitions := range consumerMap {
		offsetList[topic] = make([][]ConsumerOffset, len(partitions))
		for partition, offsetRing := range partitions {
			status.TotalPartitions += 1

			// If we don't have our ring full yet, make sure we let the caller know
			if (offsetRing == nil) || (offsetRing.Value == nil) {
				status.Complete = false
				continue
			}

			// Add an artificial offset commit if the consumer has no lag against the current broker offset
			lastOffset := offsetRing.Prev().Value.(*ConsumerOffset)
			if lastOffset.Offset >= clusterMap.broker[topic][partition].Offset {
				ringval, _ := offsetRing.Value.(*ConsumerOffset)
				ringval.Offset = lastOffset.Offset
				ringval.Timestamp = time.Now().Unix() * 1000
				ringval.Lag = 0
				ringval.artificial = true
				partitions[partition] = partitions[partition].Next()

				log.Tracef("Artificial offset: cluster=%s topic=%s partition=%v group=%s timestamp=%v offset=%v lag=0",
					cluster, topic, partition, group, ringval.Timestamp, lastOffset.Offset)
			}

			// Pull out the offsets once so we can unlock the map
			offsetList[topic][partition] = make([]ConsumerOffset, storage.app.Config.Lagcheck.Intervals)
			partitionMap := offsetList[topic][partition]
			idx := -1
			partitions[partition].Do(func(val interface{}) {
				idx += 1
				ptr, _ := val.(*ConsumerOffset)
				partitionMap[idx] = *ptr

				// Track the youngest offset we have found to check expiration
				if partitionMap[idx].Timestamp > youngestOffset {
					youngestOffset = partitionMap[idx].Timestamp
				}
			})
		}
	}

	// If the youngest offset is earlier than our expiration window, flush the group
	if (youngestOffset > 0) && (youngestOffset < ((time.Now().Unix() - storage.app.Config.Lagcheck.ExpireGroup) * 1000)) {
		log.Infof("Removing expired group %s from cluster %s", group, cluster)
		delete(clusterMap.consumer, group)
		clusterMap.consumerLock.Unlock()

		// Return the group as a 404
		status.Status = StatusNotFound
		resultChannel <- status
		return
	}
	clusterMap.consumerLock.Unlock()

	var maxlag int64
	for topic, partitions := range offsetList {
		for partition, offsets := range partitions {
			// Skip partitions we're missing offsets for
			if len(offsets) == 0 {
				continue
			}
			maxidx := len(offsets) - 1
			firstOffset := offsets[0]
			lastOffset := offsets[maxidx]

			// Rule 5 - we're missing broker offsets so we're not complete yet
			if firstOffset.Lag == -1 {
				status.Complete = false
				continue
			}

			// We may always add this partition, so create it once
			thispart := &PartitionStatus{
				Topic:     topic,
				Partition: int32(partition),
				Status:    StatusOK,
				Start:     firstOffset,
				End:       lastOffset,
			}

			// Check if this partition is the one with the most lag currently
			if lastOffset.Lag > maxlag {
				status.Maxlag = thispart
				maxlag = lastOffset.Lag
			}
			status.TotalLag += uint64(lastOffset.Lag)

			// Rule 4 - Offsets haven't been committed in a while
			if ((time.Now().Unix() * 1000) - lastOffset.Timestamp) > (lastOffset.Timestamp - firstOffset.Timestamp) {
				status.Status = StatusError
				thispart.Status = StatusStop
				status.Partitions = append(status.Partitions, thispart)
				continue
			}

			// Rule 6 - Did the consumer offsets rewind at any point?
			// We check this first because we always want to know about a rewind - it's bad behavior
			for i := 1; i <= maxidx; i++ {
				if offsets[i].Offset < offsets[i-1].Offset {
					status.Status = StatusError
					thispart.Status = StatusRewind
					status.Partitions = append(status.Partitions, thispart)
					continue
				}
			}

			// Rule 1
			if lastOffset.Lag == 0 {
				if showall {
					status.Partitions = append(status.Partitions, thispart)
				}
				continue
			}
			if lastOffset.Offset == firstOffset.Offset {
				// Rule 1
				if firstOffset.Lag == 0 {
					if showall {
						status.Partitions = append(status.Partitions, thispart)
					}
					continue
				}

				// Rule 2
				status.Status = StatusError
				thispart.Status = StatusStall
			} else {
				// Rule 1 passes, or shortcut a full check on Rule 3 if we can
				if (firstOffset.Lag == 0) || (lastOffset.Lag <= firstOffset.Lag) {
					if showall {
						status.Partitions = append(status.Partitions, thispart)
					}
					continue
				}

				lagDropped := false
				for i := 0; i <= maxidx; i++ {
					// Rule 1 passes or Rule 3 is shortcut (lag dropped somewhere in the period)
					if (offsets[i].Lag == 0) || ((i > 0) && (offsets[i].Lag < offsets[i-1].Lag)) {
						lagDropped = true
						break
					}
				}

				if !lagDropped {
					// Rule 3
					if status.Status == StatusOK {
						status.Status = StatusWarning
					}
					thispart.Status = StatusWarning
				}
			}

			// Always add the partition if it's not OK
			if (thispart.Status != StatusOK) || showall {
				status.Partitions = append(status.Partitions, thispart)
			}
		}
	}
	resultChannel <- status
}

func (storage *OffsetStorage) requestClusterList(request *RequestClusterList) {
	clusterList := make([]string, len(storage.offsets))
	i := 0
	for group, _ := range storage.offsets {
		clusterList[i] = group
		i += 1
	}

	request.Result <- clusterList
}

func (storage *OffsetStorage) requestConsumerList(request *RequestConsumerList) {
	if _, ok := storage.offsets[request.Cluster]; !ok {
		request.Result <- make([]string, 0)
		return
	}

	storage.offsets[request.Cluster].consumerLock.RLock()
	consumerList := make([]string, len(storage.offsets[request.Cluster].consumer))
	i := 0
	for group := range storage.offsets[request.Cluster].consumer {
		consumerList[i] = group
		i += 1
	}
	storage.offsets[request.Cluster].consumerLock.RUnlock()

	request.Result <- consumerList
}

func (storage *OffsetStorage) requestTopicList(request *RequestTopicList) {
	if _, ok := storage.offsets[request.Cluster]; !ok {
		request.Result <- &ResponseTopicList{Error: true}
		return
	}

	response := &ResponseTopicList{Error: false}
	if request.Group == "" {
		storage.offsets[request.Cluster].brokerLock.RLock()
		response.TopicList = make([]string, len(storage.offsets[request.Cluster].broker))
		i := 0
		for topic := range storage.offsets[request.Cluster].broker {
			response.TopicList[i] = topic
			i += 1
		}
		storage.offsets[request.Cluster].brokerLock.RUnlock()
	} else {
		storage.offsets[request.Cluster].consumerLock.RLock()
		if _, ok := storage.offsets[request.Cluster].consumer[request.Group]; ok {
			response.TopicList = make([]string, len(storage.offsets[request.Cluster].consumer[request.Group]))
			i := 0
			for topic := range storage.offsets[request.Cluster].consumer[request.Group] {
				response.TopicList[i] = topic
				i += 1
			}
		} else {
			response.Error = true
		}
		storage.offsets[request.Cluster].consumerLock.RUnlock()
	}
	request.Result <- response
}

func (storage *OffsetStorage) requestOffsets(request *RequestOffsets) {
	if _, ok := storage.offsets[request.Cluster]; !ok {
		request.Result <- &ResponseOffsets{ErrorTopic: true, ErrorGroup: true}
		return
	}

	response := &ResponseOffsets{ErrorGroup: false, ErrorTopic: false}
	if request.Group == "" {
		storage.offsets[request.Cluster].brokerLock.RLock()
		if _, ok := storage.offsets[request.Cluster].broker[request.Topic]; ok {
			response.OffsetList = make([]int64, len(storage.offsets[request.Cluster].broker[request.Topic]))
			for partition, offset := range storage.offsets[request.Cluster].broker[request.Topic] {
				if offset == nil {
					response.OffsetList[partition] = -1
				} else {
					response.OffsetList[partition] = offset.Offset
				}
			}
		} else {
			response.ErrorTopic = true
		}
		storage.offsets[request.Cluster].brokerLock.RUnlock()
	} else {
		storage.offsets[request.Cluster].consumerLock.RLock()
		if _, ok := storage.offsets[request.Cluster].consumer[request.Group]; ok {
			if _, ok := storage.offsets[request.Cluster].consumer[request.Group][request.Topic]; ok {
				response.OffsetList = make([]int64, len(storage.offsets[request.Cluster].consumer[request.Group][request.Topic]))
				for partition, oring := range storage.offsets[request.Cluster].consumer[request.Group][request.Topic] {
					if oring == nil {
						response.OffsetList[partition] = -1
					} else {
						offset, _ := oring.Prev().Value.(*ConsumerOffset)
						if offset == nil {
							response.OffsetList[partition] = -1
						} else {
							response.OffsetList[partition] = offset.Offset
						}
					}
				}
			} else {
				response.ErrorTopic = true
			}
		} else {
			response.ErrorGroup = true
		}
		storage.offsets[request.Cluster].consumerLock.RUnlock()
	}
	request.Result <- response
}

func (storage *OffsetStorage) debugPrintGroup(cluster string, group string) {
	// Make sure the cluster exists
	clusterMap, ok := storage.offsets[cluster]
	if !ok {
		log.Debugf("Detail cluster=%s,group=%s: No Cluster", cluster, group)
		return
	}

	// Make sure the group even exists
	clusterMap.consumerLock.RLock()
	consumerMap, ok := clusterMap.consumer[group]
	if !ok {
		clusterMap.consumerLock.RUnlock()
		log.Debugf("Detail cluster=%s,group=%s: No Group", cluster, group)
		return
	}

	// Scan the offsets table and print all partitions (full ring) for the group
	for topic, partitions := range consumerMap {
		for partition, offsetRing := range partitions {
			if (offsetRing == nil) || (offsetRing.Value == nil) {
				log.Debugf("Detail cluster=%s,group=%s,topic=%s,partition=%v: No Ring", cluster, group, topic, partition)
				continue
			}

			// Pull out the offsets once so we can unlock the map
			ringStr := ""
			offsetRing.Do(func(val interface{}) {
				if val == nil {
					ringStr += "(),"
				} else {
					ptr, _ := val.(*ConsumerOffset)
					ringStr += fmt.Sprintf("(%v,%v,%v,%v)", ptr.Timestamp, ptr.Offset, ptr.Lag, ptr.artificial)
				}
			})
			log.Debugf("Detail cluster=%s,group=%s,topic=%s,partition=%v: %s", cluster, group, topic, partition, ringStr)
		}
	}
	clusterMap.consumerLock.RUnlock()
}
