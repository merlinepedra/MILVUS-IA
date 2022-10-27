// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package observers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/suite"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/milvus-io/milvus-proto/go-api/commonpb"
	etcdkv "github.com/milvus-io/milvus/internal/kv/etcd"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	. "github.com/milvus-io/milvus/internal/querycoordv2/params"
	"github.com/milvus-io/milvus/internal/util"
	"github.com/milvus-io/milvus/internal/util/etcd"
	"github.com/milvus-io/milvus/internal/util/typeutil"
)

const (
	defaultVecFieldID = 1
	defaultIndexID    = 1
)

type HandoffObserverTestSuit struct {
	suite.Suite
	// Data
	collection      int64
	partition       int64
	channel         *meta.DmChannel
	replicaNumber   int32
	nodes           []int64
	growingSegments []*datapb.SegmentInfo
	sealedSegments  []*datapb.SegmentInfo

	//Mocks
	idAllocator func() (int64, error)
	etcd        *clientv3.Client
	kv          *etcdkv.EtcdKV

	//Dependency
	store  meta.Store
	meta   *meta.Meta
	dist   *meta.DistributionManager
	target *meta.TargetManager

	// Test Object
	observer *HandoffObserver
}

func (suite *HandoffObserverTestSuit) SetupSuite() {
	Params.Init()

	suite.collection = 100
	suite.partition = 10
	suite.channel = meta.DmChannelFromVChannel(&datapb.VchannelInfo{
		CollectionID: 100,
		ChannelName:  "100-dmc0",
	})

	suite.sealedSegments = []*datapb.SegmentInfo{
		{
			ID:            1,
			CollectionID:  100,
			PartitionID:   10,
			InsertChannel: "100-dmc0",
			State:         commonpb.SegmentState_Sealed,
		},
		{
			ID:            2,
			CollectionID:  100,
			PartitionID:   10,
			InsertChannel: "100-dmc1",
			State:         commonpb.SegmentState_Sealed,
		},
	}
	suite.replicaNumber = 1
	suite.nodes = []int64{1, 2, 3}
}

func (suite *HandoffObserverTestSuit) SetupTest() {
	// Mocks
	var err error
	suite.idAllocator = RandomIncrementIDAllocator()
	log.Debug("create embedded etcd KV...")
	config := GenerateEtcdConfig()
	client, err := etcd.GetEtcdClient(&config)
	suite.Require().NoError(err)
	suite.kv = etcdkv.NewEtcdKV(client, Params.EtcdCfg.MetaRootPath+"-"+RandomMetaRootPath())
	suite.Require().NoError(err)
	log.Debug("create meta store...")
	suite.store = meta.NewMetaStore(suite.kv)

	// Dependency
	suite.meta = meta.NewMeta(suite.idAllocator, suite.store)
	suite.dist = meta.NewDistributionManager()
	suite.target = meta.NewTargetManager()

	// Test Object
	suite.observer = NewHandoffObserver(suite.store, suite.meta, suite.dist, suite.target)
	suite.observer.Register(suite.collection)
	suite.observer.StartHandoff(suite.collection)
	suite.load()
}

func (suite *HandoffObserverTestSuit) TearDownTest() {
	suite.observer.Stop()
	suite.kv.Close()
}

func (suite *HandoffObserverTestSuit) TestFlushingHandoff() {
	// init leader view
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:              1,
		CollectionID:    suite.collection,
		Channel:         suite.channel.ChannelName,
		Segments:        map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}},
		GrowingSegments: typeutil.NewUniqueSet(3),
	})

	Params.QueryCoordCfg.CheckHandoffInterval = 1 * time.Second
	err := suite.observer.Start(context.Background())
	suite.NoError(err)

	flushingSegment := &querypb.SegmentInfo{
		SegmentID:    3,
		CollectionID: suite.collection,
		PartitionID:  suite.partition,
		SegmentState: commonpb.SegmentState_Sealed,
		IndexInfos:   []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}
	suite.produceHandOffEvent(flushingSegment)

	suite.Eventually(func() bool {
		return suite.target.ContainSegment(3)
	}, 3*time.Second, 1*time.Second)

	// fake load CompactTo Segment
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:              1,
		CollectionID:    suite.collection,
		Channel:         suite.channel.ChannelName,
		Segments:        map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}, 3: {NodeID: 3, Version: 0}},
		GrowingSegments: typeutil.NewUniqueSet(3),
	})

	// fake release CompactFrom Segment
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:           1,
		CollectionID: suite.collection,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}, 3: {NodeID: 3, Version: 0}},
	})

	suite.Eventually(func() bool {
		return len(suite.dist.LeaderViewManager.GetGrowingSegmentDist(3)) == 0
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		key := fmt.Sprintf("%s/%d/%d/%d", util.HandoffSegmentPrefix, suite.collection, suite.partition, 3)
		value, err := suite.kv.Load(key)
		return len(value) == 0 && err != nil
	}, 3*time.Second, 1*time.Second)
}

func (suite *HandoffObserverTestSuit) TestCompactHandoff() {
	// init leader view
	suite.dist.LeaderViewManager.Update(2, &meta.LeaderView{
		ID:           1,
		CollectionID: suite.collection,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}},
	})

	Params.QueryCoordCfg.CheckHandoffInterval = 1 * time.Second
	err := suite.observer.Start(context.Background())
	suite.NoError(err)

	compactSegment := &querypb.SegmentInfo{
		SegmentID:           3,
		CollectionID:        suite.collection,
		PartitionID:         suite.partition,
		SegmentState:        commonpb.SegmentState_Sealed,
		CompactionFrom:      []int64{1},
		CreatedByCompaction: true,
		IndexInfos:          []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}
	suite.produceHandOffEvent(compactSegment)

	suite.Eventually(func() bool {
		return suite.target.ContainSegment(3)
	}, 3*time.Second, 1*time.Second)

	// fake load CompactTo Segment
	suite.dist.LeaderViewManager.Update(2, &meta.LeaderView{
		ID:           1,
		CollectionID: suite.collection,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}, 3: {NodeID: 3, Version: 0}},
	})

	suite.Eventually(func() bool {
		return !suite.target.ContainSegment(1)
	}, 3*time.Second, 1*time.Second)

	// fake release CompactFrom Segment
	suite.dist.LeaderViewManager.Update(2, &meta.LeaderView{
		ID:           1,
		CollectionID: suite.collection,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{2: {NodeID: 2, Version: 0}, 3: {NodeID: 3, Version: 0}},
	})

	suite.Eventually(func() bool {
		key := fmt.Sprintf("%s/%d/%d/%d", util.HandoffSegmentPrefix, suite.collection, suite.partition, 3)
		value, err := suite.kv.Load(key)
		return len(value) == 0 && err != nil
	}, 3*time.Second, 1*time.Second)
}

func (suite *HandoffObserverTestSuit) TestRecursiveHandoff() {
	// init leader view
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:              1,
		CollectionID:    suite.collection,
		Channel:         suite.channel.ChannelName,
		Segments:        map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}},
		GrowingSegments: typeutil.NewUniqueSet(3),
	})

	Params.QueryCoordCfg.CheckHandoffInterval = 1 * time.Second
	err := suite.observer.Start(context.Background())
	suite.NoError(err)

	flushingSegment := &querypb.SegmentInfo{
		SegmentID:    3,
		CollectionID: suite.collection,
		PartitionID:  suite.partition,
		SegmentState: commonpb.SegmentState_Sealed,
		IndexInfos:   []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}

	compactSegment1 := &querypb.SegmentInfo{
		SegmentID:           4,
		CollectionID:        suite.collection,
		PartitionID:         suite.partition,
		SegmentState:        commonpb.SegmentState_Sealed,
		CompactionFrom:      []int64{3},
		CreatedByCompaction: true,
		IndexInfos:          []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}

	compactSegment2 := &querypb.SegmentInfo{
		SegmentID:           5,
		CollectionID:        suite.collection,
		PartitionID:         suite.partition,
		SegmentState:        commonpb.SegmentState_Sealed,
		CompactionFrom:      []int64{4},
		CreatedByCompaction: true,
		IndexInfos:          []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}

	suite.produceHandOffEvent(flushingSegment)
	suite.produceHandOffEvent(compactSegment1)
	suite.produceHandOffEvent(compactSegment2)

	// fake load CompactTo Segment
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:              1,
		CollectionID:    suite.collection,
		Channel:         suite.channel.ChannelName,
		Segments:        map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}, 5: {NodeID: 3, Version: 0}},
		GrowingSegments: typeutil.NewUniqueSet(3),
	})

	suite.Eventually(func() bool {
		return suite.target.ContainSegment(1) && suite.target.ContainSegment(2) && suite.target.ContainSegment(5)
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		return !suite.target.ContainSegment(3) && !suite.target.ContainSegment(4)
	}, 3*time.Second, 1*time.Second)

	// fake release CompactFrom Segment
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:           1,
		CollectionID: suite.collection,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}, 5: {NodeID: 3, Version: 0}},
	})

	suite.Eventually(func() bool {
		return suite.target.ContainSegment(1) && suite.target.ContainSegment(2) && suite.target.ContainSegment(5)
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		return !suite.target.ContainSegment(3) && !suite.target.ContainSegment(4)
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		return len(suite.dist.LeaderViewManager.GetGrowingSegmentDist(3)) == 0
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		key := fmt.Sprintf("%s/%d/%d/%d", util.HandoffSegmentPrefix, suite.collection, suite.partition, 3)
		value, err := suite.kv.Load(key)
		return len(value) == 0 && err != nil
	}, 3*time.Second, 1*time.Second)
}

func (suite *HandoffObserverTestSuit) TestReloadHandoffEventOrder() {
	// init leader view
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:              1,
		CollectionID:    suite.collection,
		Channel:         suite.channel.ChannelName,
		Segments:        map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}},
		GrowingSegments: typeutil.NewUniqueSet(3),
	})

	// fake handoff event from start
	flushingSegment := &querypb.SegmentInfo{
		SegmentID:    3,
		CollectionID: suite.collection,
		PartitionID:  suite.partition,
		SegmentState: commonpb.SegmentState_Sealed,
		IndexInfos:   []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}
	compactSegment1 := &querypb.SegmentInfo{
		SegmentID:           9,
		CollectionID:        suite.collection,
		PartitionID:         suite.partition,
		SegmentState:        commonpb.SegmentState_Sealed,
		CompactionFrom:      []int64{3},
		CreatedByCompaction: true,
		IndexInfos:          []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}
	compactSegment2 := &querypb.SegmentInfo{
		SegmentID:           10,
		CollectionID:        suite.collection,
		PartitionID:         suite.partition,
		SegmentState:        commonpb.SegmentState_Sealed,
		CompactionFrom:      []int64{4},
		CreatedByCompaction: true,
		IndexInfos:          []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}

	suite.produceHandOffEvent(flushingSegment)
	suite.produceHandOffEvent(compactSegment1)
	suite.produceHandOffEvent(compactSegment2)

	keys, _, _, err := suite.kv.LoadWithRevision(util.HandoffSegmentPrefix)
	suite.NoError(err)
	suite.Equal(true, strings.HasSuffix(keys[0], "3"))
	suite.Equal(true, strings.HasSuffix(keys[1], "9"))
	suite.Equal(true, strings.HasSuffix(keys[2], "10"))
}

func (suite *HandoffObserverTestSuit) TestLoadHandoffEventFromStore() {
	// init leader view
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:              1,
		CollectionID:    suite.collection,
		Channel:         suite.channel.ChannelName,
		Segments:        map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}},
		GrowingSegments: typeutil.NewUniqueSet(3),
	})

	// fake handoff event from start
	flushingSegment := &querypb.SegmentInfo{
		SegmentID:    3,
		CollectionID: suite.collection,
		PartitionID:  suite.partition,
		SegmentState: commonpb.SegmentState_Sealed,
		IndexInfos:   []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}
	compactSegment1 := &querypb.SegmentInfo{
		SegmentID:           4,
		CollectionID:        suite.collection,
		PartitionID:         suite.partition,
		SegmentState:        commonpb.SegmentState_Sealed,
		CompactionFrom:      []int64{3},
		CreatedByCompaction: true,
		IndexInfos:          []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}
	compactSegment2 := &querypb.SegmentInfo{
		SegmentID:           5,
		CollectionID:        suite.collection,
		PartitionID:         suite.partition,
		SegmentState:        commonpb.SegmentState_Sealed,
		CompactionFrom:      []int64{4},
		CreatedByCompaction: true,
		IndexInfos:          []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}

	suite.produceHandOffEvent(flushingSegment)
	suite.produceHandOffEvent(compactSegment1)
	suite.produceHandOffEvent(compactSegment2)

	Params.QueryCoordCfg.CheckHandoffInterval = 1 * time.Second
	err := suite.observer.Start(context.Background())
	suite.NoError(err)

	// fake load CompactTo Segment
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:              1,
		CollectionID:    suite.collection,
		Channel:         suite.channel.ChannelName,
		Segments:        map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}, 3: {NodeID: 3, Version: 0}, 4: {NodeID: 2, Version: 0}, 5: {NodeID: 3, Version: 0}},
		GrowingSegments: typeutil.NewUniqueSet(3),
	})

	suite.Eventually(func() bool {
		return suite.target.ContainSegment(1) && suite.target.ContainSegment(2) && suite.target.ContainSegment(5)
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		return !suite.target.ContainSegment(3) && !suite.target.ContainSegment(4)
	}, 3*time.Second, 1*time.Second)

	// fake release CompactFrom Segment
	suite.dist.LeaderViewManager.Update(1, &meta.LeaderView{
		ID:           1,
		CollectionID: suite.collection,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}, 5: {NodeID: 3, Version: 0}},
	})

	suite.Eventually(func() bool {
		return suite.target.ContainSegment(1) && suite.target.ContainSegment(2) && suite.target.ContainSegment(5)
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		return !suite.target.ContainSegment(3) && !suite.target.ContainSegment(4)
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		return len(suite.dist.LeaderViewManager.GetGrowingSegmentDist(3)) == 0
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		key := fmt.Sprintf("%s/%d/%d/%d", util.HandoffSegmentPrefix, suite.collection, suite.partition, 3)
		value, err := suite.kv.Load(key)
		return len(value) == 0 && err != nil
	}, 3*time.Second, 1*time.Second)
}

func (suite *HandoffObserverTestSuit) produceHandOffEvent(segmentInfo *querypb.SegmentInfo) {
	key := fmt.Sprintf("%s/%d/%d/%d", util.HandoffSegmentPrefix, segmentInfo.CollectionID, segmentInfo.PartitionID, segmentInfo.SegmentID)
	value, err := proto.Marshal(segmentInfo)
	suite.NoError(err)
	err = suite.kv.Save(key, string(value))
	suite.NoError(err)
}

func (suite *HandoffObserverTestSuit) existHandOffEvent(segmentInfo *querypb.SegmentInfo) bool {
	key := fmt.Sprintf("%s/%d/%d/%d", util.HandoffSegmentPrefix, segmentInfo.CollectionID, segmentInfo.PartitionID, segmentInfo.SegmentID)
	_, err := suite.kv.Load(key)
	return err == nil
}

func (suite *HandoffObserverTestSuit) load() {
	// Mock meta data
	replicas, err := suite.meta.ReplicaManager.Spawn(suite.collection, suite.replicaNumber)
	suite.NoError(err)
	for _, replica := range replicas {
		replica.AddNode(suite.nodes...)
	}
	err = suite.meta.ReplicaManager.Put(replicas...)
	suite.NoError(err)

	err = suite.meta.PutCollection(&meta.Collection{
		CollectionLoadInfo: &querypb.CollectionLoadInfo{
			CollectionID:  suite.collection,
			ReplicaNumber: suite.replicaNumber,
			Status:        querypb.LoadStatus_Loaded,
			FieldIndexID:  map[int64]int64{defaultVecFieldID: defaultIndexID},
		},
		LoadPercentage: 0,
		CreatedAt:      time.Now(),
	})
	suite.NoError(err)

	suite.target.AddDmChannel(suite.channel)
	suite.target.AddSegment(suite.sealedSegments...)
}

func (suite *HandoffObserverTestSuit) TestHandoffOnUnLoadedPartition() {
	const (
		collectionID        = 111
		loadedPartitionID   = 1
		unloadedPartitionID = 2
	)
	err := suite.meta.PutPartition(&meta.Partition{
		PartitionLoadInfo: &querypb.PartitionLoadInfo{
			CollectionID:  collectionID,
			PartitionID:   loadedPartitionID,
			ReplicaNumber: suite.replicaNumber,
			Status:        querypb.LoadStatus_Loaded,
		},
	})
	suite.NoError(err)

	// init leader view
	suite.dist.LeaderViewManager.Update(2, &meta.LeaderView{
		ID:           1,
		CollectionID: collectionID,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}},
	})

	Params.QueryCoordCfg.CheckHandoffInterval = 1 * time.Second
	err = suite.observer.Start(context.Background())
	suite.NoError(err)

	compactSegment := &querypb.SegmentInfo{
		SegmentID:           3,
		CollectionID:        collectionID,
		PartitionID:         unloadedPartitionID,
		SegmentState:        commonpb.SegmentState_Sealed,
		CompactionFrom:      []int64{2},
		CreatedByCompaction: true,
		IndexInfos:          []*querypb.FieldIndexInfo{{IndexID: defaultIndexID}},
	}
	suite.produceHandOffEvent(compactSegment)

	suite.Eventually(func() bool {
		return suite.target.ContainSegment(1) && suite.target.ContainSegment(2)
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		return !suite.target.ContainSegment(3)
	}, 3*time.Second, 1*time.Second)

	suite.Eventually(func() bool {
		key := fmt.Sprintf("%s/%d/%d/%d", util.HandoffSegmentPrefix, suite.collection, suite.partition, 3)
		value, err := suite.kv.Load(key)
		return len(value) == 0 && err != nil
	}, 3*time.Second, 1*time.Second)
}

func (suite *HandoffObserverTestSuit) TestFilterOutEventByIndexID() {
	// init leader view
	suite.dist.LeaderViewManager.Update(2, &meta.LeaderView{
		ID:           1,
		CollectionID: suite.collection,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}},
	})

	Params.QueryCoordCfg.CheckHandoffInterval = 1 * time.Second
	err := suite.observer.Start(context.Background())
	suite.NoError(err)

	compactSegment := &querypb.SegmentInfo{
		SegmentID:           3,
		CollectionID:        suite.collection,
		PartitionID:         suite.partition,
		SegmentState:        commonpb.SegmentState_Sealed,
		CompactionFrom:      []int64{1},
		CreatedByCompaction: true,
	}
	suite.produceHandOffEvent(compactSegment)

	suite.Eventually(func() bool {
		suite.observer.handoffEventLock.RLock()
		defer suite.observer.handoffEventLock.RUnlock()
		_, ok := suite.observer.handoffEvents[compactSegment.GetSegmentID()]
		return !ok && !suite.target.ContainSegment(3) && !suite.existHandOffEvent(compactSegment)
	}, 3*time.Second, 1*time.Second)
}

func (suite *HandoffObserverTestSuit) TestFakedSegmentHandoff() {
	suite.dist.LeaderViewManager.Update(2, &meta.LeaderView{
		ID:           1,
		CollectionID: suite.collection,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}},
	})

	Params.QueryCoordCfg.CheckHandoffInterval = 200 * time.Millisecond
	err := suite.observer.Start(context.Background())
	suite.NoError(err)

	handoffSegment := &querypb.SegmentInfo{
		SegmentID:           3,
		CollectionID:        suite.collection,
		PartitionID:         suite.partition,
		CompactionFrom:      []int64{1, 2},
		CreatedByCompaction: true,
		IsFake:              true,
	}
	suite.produceHandOffEvent(handoffSegment)

	time.Sleep(1 * time.Second)
	suite.dist.LeaderViewManager.Update(2, &meta.LeaderView{
		ID:           1,
		CollectionID: suite.collection,
		Channel:      suite.channel.ChannelName,
		Segments:     map[int64]*querypb.SegmentDist{1: {NodeID: 1, Version: 0}, 2: {NodeID: 2, Version: 0}},
	})

	suite.Eventually(func() bool {
		return !suite.target.ContainSegment(1) && !suite.target.ContainSegment(2)
	}, 3*time.Second, 1*time.Second)
}

func TestHandoffObserverSuit(t *testing.T) {
	suite.Run(t, new(HandoffObserverTestSuit))
}