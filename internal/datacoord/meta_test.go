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

package datacoord

import (
	"context"
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/msgpb"
	"github.com/milvus-io/milvus/internal/kv"
	mockkv "github.com/milvus-io/milvus/internal/kv/mocks"
	"github.com/milvus-io/milvus/internal/metastore/kv/datacoord"
	"github.com/milvus-io/milvus/internal/metastore/model"
	"github.com/milvus-io/milvus/internal/mocks"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/metrics"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/testutils"
)

// MetaReloadSuite tests meta reload & meta creation related logic
type MetaReloadSuite struct {
	testutils.PromMetricsSuite

	catalog *mocks.DataCoordCatalog
	meta    *meta
}

func (suite *MetaReloadSuite) SetupTest() {
	catalog := mocks.NewDataCoordCatalog(suite.T())
	suite.catalog = catalog
}

func (suite *MetaReloadSuite) resetMock() {
	suite.catalog.ExpectedCalls = nil
}

func (suite *MetaReloadSuite) TestReloadFromKV() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	suite.Run("ListSegments_fail", func() {
		defer suite.resetMock()
		suite.catalog.EXPECT().ListSegments(mock.Anything).Return(nil, errors.New("mock"))

		_, err := newMeta(ctx, suite.catalog, nil)
		suite.Error(err)
	})

	suite.Run("ListChannelCheckpoint_fail", func() {
		defer suite.resetMock()

		suite.catalog.EXPECT().ListSegments(mock.Anything).Return([]*datapb.SegmentInfo{}, nil)
		suite.catalog.EXPECT().ListChannelCheckpoint(mock.Anything).Return(nil, errors.New("mock"))

		_, err := newMeta(ctx, suite.catalog, nil)
		suite.Error(err)
	})

	suite.Run("ListIndexes_fail", func() {
		defer suite.resetMock()

		suite.catalog.EXPECT().ListSegments(mock.Anything).Return([]*datapb.SegmentInfo{}, nil)
		suite.catalog.EXPECT().ListChannelCheckpoint(mock.Anything).Return(map[string]*msgpb.MsgPosition{}, nil)
		suite.catalog.EXPECT().ListIndexes(mock.Anything).Return(nil, errors.New("mock"))

		_, err := newMeta(ctx, suite.catalog, nil)
		suite.Error(err)
	})

	suite.Run("ListSegmentIndexes_fails", func() {
		defer suite.resetMock()

		suite.catalog.EXPECT().ListSegments(mock.Anything).Return([]*datapb.SegmentInfo{}, nil)
		suite.catalog.EXPECT().ListChannelCheckpoint(mock.Anything).Return(map[string]*msgpb.MsgPosition{}, nil)
		suite.catalog.EXPECT().ListIndexes(mock.Anything).Return([]*model.Index{}, nil)
		suite.catalog.EXPECT().ListSegmentIndexes(mock.Anything).Return(nil, errors.New("mock"))

		_, err := newMeta(ctx, suite.catalog, nil)
		suite.Error(err)
	})

	suite.Run("ok", func() {
		defer suite.resetMock()

		suite.catalog.EXPECT().ListSegments(mock.Anything).Return([]*datapb.SegmentInfo{
			{
				ID:           1,
				CollectionID: 1,
				PartitionID:  1,
				State:        commonpb.SegmentState_Flushed,
			},
		}, nil)
		suite.catalog.EXPECT().ListChannelCheckpoint(mock.Anything).Return(map[string]*msgpb.MsgPosition{
			"ch": {
				ChannelName: "cn",
				MsgID:       []byte{},
				Timestamp:   1000,
			},
		}, nil)
		suite.catalog.EXPECT().ListIndexes(mock.Anything).Return([]*model.Index{
			{
				CollectionID: 1,
				IndexID:      1,
				IndexName:    "dix",
				CreateTime:   1,
			},
		}, nil)

		suite.catalog.EXPECT().ListSegmentIndexes(mock.Anything).Return([]*model.SegmentIndex{
			{
				SegmentID: 1,
				IndexID:   1,
			},
		}, nil)

		meta, err := newMeta(ctx, suite.catalog, nil)
		suite.NoError(err)
		suite.NotNil(meta)

		suite.MetricsEqual(metrics.DataCoordNumSegments.WithLabelValues(metrics.FlushedSegmentLabel, datapb.SegmentLevel_Legacy.String()), 1)
	})
}

type MetaBasicSuite struct {
	testutils.PromMetricsSuite

	collID      int64
	partIDs     []int64
	channelName string

	meta *meta
}

func (suite *MetaBasicSuite) SetupSuite() {
	paramtable.Init()
}

func (suite *MetaBasicSuite) SetupTest() {
	suite.collID = 1
	suite.partIDs = []int64{100, 101}
	suite.channelName = "c1"

	meta, err := newMemoryMeta()

	suite.Require().NoError(err)
	suite.meta = meta
}

func (suite *MetaBasicSuite) getCollectionInfo(partIDs ...int64) *collectionInfo {
	testSchema := newTestSchema()
	return &collectionInfo{
		ID:             suite.collID,
		Schema:         testSchema,
		Partitions:     partIDs,
		StartPositions: []*commonpb.KeyDataPair{},
	}
}

func (suite *MetaBasicSuite) TestCollection() {
	meta := suite.meta

	info := suite.getCollectionInfo(suite.partIDs...)
	meta.AddCollection(info)

	collInfo := meta.GetCollection(suite.collID)
	suite.Require().NotNil(collInfo)

	// check partition info
	suite.EqualValues(suite.collID, collInfo.ID)
	suite.EqualValues(info.Schema, collInfo.Schema)
	suite.EqualValues(len(suite.partIDs), len(collInfo.Partitions))
	suite.ElementsMatch(info.Partitions, collInfo.Partitions)

	suite.MetricsEqual(metrics.DataCoordNumCollections.WithLabelValues(), 1)
}

func (suite *MetaBasicSuite) TestCompleteCompactionMutation() {
	latestSegments := &SegmentsInfo{
		map[UniqueID]*SegmentInfo{
			1: {SegmentInfo: &datapb.SegmentInfo{
				ID:           1,
				CollectionID: 100,
				PartitionID:  10,
				State:        commonpb.SegmentState_Flushed,
				Level:        datapb.SegmentLevel_L1,
				Binlogs:      []*datapb.FieldBinlog{getFieldBinlogIDs(0, 10000, 10001)},
				Statslogs:    []*datapb.FieldBinlog{getFieldBinlogIDs(0, 20000, 20001)},
				// latest segment has 2 deltalogs, one submit for compaction, one is appended before compaction done
				Deltalogs: []*datapb.FieldBinlog{getFieldBinlogIDs(0, 30000), getFieldBinlogIDs(0, 30001)},
				NumOfRows: 2,
			}},
			2: {SegmentInfo: &datapb.SegmentInfo{
				ID:           2,
				CollectionID: 100,
				PartitionID:  10,
				State:        commonpb.SegmentState_Flushed,
				Level:        datapb.SegmentLevel_L1,
				Binlogs:      []*datapb.FieldBinlog{getFieldBinlogIDs(0, 11000)},
				Statslogs:    []*datapb.FieldBinlog{getFieldBinlogIDs(0, 21000)},
				// latest segment has 2 deltalogs, one submit for compaction, one is appended before compaction done
				Deltalogs: []*datapb.FieldBinlog{getFieldBinlogIDs(0, 31000), getFieldBinlogIDs(0, 31001)},
				NumOfRows: 2,
			}},
		},
	}

	mockChMgr := mocks.NewChunkManager(suite.T())
	mockChMgr.EXPECT().RootPath().Return("mockroot").Times(4)
	mockChMgr.EXPECT().Read(mock.Anything, mock.Anything).Return(nil, nil).Twice()
	mockChMgr.EXPECT().Write(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()

	m := &meta{
		catalog:      &datacoord.Catalog{MetaKv: NewMetaMemoryKV()},
		segments:     latestSegments,
		chunkManager: mockChMgr,
	}

	plan := &datapb.CompactionPlan{
		SegmentBinlogs: []*datapb.CompactionSegmentBinlogs{
			{
				SegmentID:           1,
				FieldBinlogs:        m.GetSegment(1).GetBinlogs(),
				Field2StatslogPaths: m.GetSegment(1).GetStatslogs(),
				Deltalogs:           m.GetSegment(1).GetDeltalogs()[:1], // compaction plan use only 1 deltalog
			},
			{
				SegmentID:           2,
				FieldBinlogs:        m.GetSegment(2).GetBinlogs(),
				Field2StatslogPaths: m.GetSegment(2).GetStatslogs(),
				Deltalogs:           m.GetSegment(2).GetDeltalogs()[:1], // compaction plan use only 1 deltalog
			},
		},
	}

	compactToSeg := &datapb.CompactionSegment{
		SegmentID:           3,
		InsertLogs:          []*datapb.FieldBinlog{getFieldBinlogIDs(0, 50000)},
		Field2StatslogPaths: []*datapb.FieldBinlog{getFieldBinlogIDs(0, 50001)},
		NumOfRows:           2,
	}

	result := &datapb.CompactionPlanResult{
		Segments: []*datapb.CompactionSegment{compactToSeg},
	}

	infos, mutation, err := m.CompleteCompactionMutation(plan, result)
	suite.Equal(1, len(infos))
	info := infos[0]
	suite.NoError(err)
	suite.NotNil(info)
	suite.NotNil(mutation)

	// check newSegment
	suite.EqualValues(3, info.GetID())
	suite.Equal(datapb.SegmentLevel_L1, info.GetLevel())
	suite.Equal(commonpb.SegmentState_Flushed, info.GetState())

	binlogs := info.GetBinlogs()
	for _, fbinlog := range binlogs {
		for _, blog := range fbinlog.GetBinlogs() {
			suite.Empty(blog.GetLogPath())
			suite.EqualValues(50000, blog.GetLogID())
		}
	}

	statslogs := info.GetStatslogs()
	for _, fbinlog := range statslogs {
		for _, blog := range fbinlog.GetBinlogs() {
			suite.Empty(blog.GetLogPath())
			suite.EqualValues(50001, blog.GetLogID())
		}
	}

	deltalogs := info.GetDeltalogs()
	deltalogIDs := []int64{}
	for _, fbinlog := range deltalogs {
		for _, blog := range fbinlog.GetBinlogs() {
			suite.Empty(blog.GetLogPath())
			deltalogIDs = append(deltalogIDs, blog.GetLogID())
		}
	}
	suite.ElementsMatch([]int64{30001, 31001}, deltalogIDs)

	// check compactFrom segments
	for _, segID := range []int64{1, 2} {
		seg := m.GetSegment(segID)
		suite.Equal(commonpb.SegmentState_Dropped, seg.GetState())
		suite.NotEmpty(seg.GetDroppedAt())

		suite.EqualValues(segID, seg.GetID())
		suite.ElementsMatch(latestSegments.segments[segID].GetBinlogs(), seg.GetBinlogs())
		suite.ElementsMatch(latestSegments.segments[segID].GetStatslogs(), seg.GetStatslogs())
		suite.ElementsMatch(latestSegments.segments[segID].GetDeltalogs(), seg.GetDeltalogs())
	}

	// check mutation metrics
	suite.Equal(2, len(mutation.stateChange[datapb.SegmentLevel_L1.String()]))
	suite.EqualValues(-2, mutation.rowCountChange)
	suite.EqualValues(2, mutation.rowCountAccChange)
}

func (suite *MetaBasicSuite) TestSetSegment() {
	meta := suite.meta
	catalog := mocks.NewDataCoordCatalog(suite.T())
	meta.catalog = catalog
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	suite.Run("normal", func() {
		segmentID := int64(1000)
		catalog.EXPECT().AddSegment(mock.Anything, mock.Anything).Return(nil).Once()
		segment := NewSegmentInfo(&datapb.SegmentInfo{
			ID:            segmentID,
			MaxRowNum:     30000,
			CollectionID:  suite.collID,
			InsertChannel: suite.channelName,
			State:         commonpb.SegmentState_Flushed,
		})
		err := meta.AddSegment(ctx, segment)
		suite.Require().NoError(err)

		noOp := func(segment *SegmentInfo) bool {
			return true
		}

		catalog.EXPECT().AlterSegments(mock.Anything, mock.Anything).Return(nil).Once()

		err = meta.UpdateSegment(segmentID, noOp)
		suite.NoError(err)
	})

	suite.Run("not_updated", func() {
		segmentID := int64(1001)
		catalog.EXPECT().AddSegment(mock.Anything, mock.Anything).Return(nil).Once()
		segment := NewSegmentInfo(&datapb.SegmentInfo{
			ID:            segmentID,
			MaxRowNum:     30000,
			CollectionID:  suite.collID,
			InsertChannel: suite.channelName,
			State:         commonpb.SegmentState_Flushed,
		})
		err := meta.AddSegment(ctx, segment)
		suite.Require().NoError(err)

		noOp := func(segment *SegmentInfo) bool {
			return false
		}

		err = meta.UpdateSegment(segmentID, noOp)
		suite.NoError(err)
	})

	suite.Run("catalog_error", func() {
		segmentID := int64(1002)
		catalog.EXPECT().AddSegment(mock.Anything, mock.Anything).Return(nil).Once()
		segment := NewSegmentInfo(&datapb.SegmentInfo{
			ID:            segmentID,
			MaxRowNum:     30000,
			CollectionID:  suite.collID,
			InsertChannel: suite.channelName,
			State:         commonpb.SegmentState_Flushed,
		})
		err := meta.AddSegment(ctx, segment)
		suite.Require().NoError(err)

		noOp := func(segment *SegmentInfo) bool {
			return true
		}

		catalog.EXPECT().AlterSegments(mock.Anything, mock.Anything).Return(errors.New("mocked")).Once()

		err = meta.UpdateSegment(segmentID, noOp)
		suite.Error(err)
	})

	suite.Run("segment_not_found", func() {
		segmentID := int64(1003)

		noOp := func(segment *SegmentInfo) bool {
			return true
		}

		err := meta.UpdateSegment(segmentID, noOp)
		suite.Error(err)
		suite.ErrorIs(err, merr.ErrSegmentNotFound)
	})
}

func TestMeta(t *testing.T) {
	suite.Run(t, new(MetaBasicSuite))
	suite.Run(t, new(MetaReloadSuite))
}

func TestMeta_Basic(t *testing.T) {
	const collID = UniqueID(0)
	const partID0 = UniqueID(100)
	const partID1 = UniqueID(101)
	const channelName = "c1"
	ctx := context.Background()

	mockAllocator := newMockAllocator()
	meta, err := newMemoryMeta()
	assert.NoError(t, err)

	testSchema := newTestSchema()

	collInfo := &collectionInfo{
		ID:             collID,
		Schema:         testSchema,
		Partitions:     []UniqueID{partID0, partID1},
		StartPositions: []*commonpb.KeyDataPair{},
	}
	collInfoWoPartition := &collectionInfo{
		ID:         collID,
		Schema:     testSchema,
		Partitions: []UniqueID{},
	}

	t.Run("Test Segment", func(t *testing.T) {
		meta.AddCollection(collInfoWoPartition)
		// create seg0 for partition0, seg0/seg1 for partition1
		segID0_0, err := mockAllocator.allocID(ctx)
		assert.NoError(t, err)
		segInfo0_0 := buildSegment(collID, partID0, segID0_0, channelName, true)
		segID1_0, err := mockAllocator.allocID(ctx)
		assert.NoError(t, err)
		segInfo1_0 := buildSegment(collID, partID1, segID1_0, channelName, false)
		segID1_1, err := mockAllocator.allocID(ctx)
		assert.NoError(t, err)
		segInfo1_1 := buildSegment(collID, partID1, segID1_1, channelName, false)

		// check AddSegment
		err = meta.AddSegment(context.TODO(), segInfo0_0)
		assert.NoError(t, err)
		err = meta.AddSegment(context.TODO(), segInfo1_0)
		assert.NoError(t, err)
		err = meta.AddSegment(context.TODO(), segInfo1_1)
		assert.NoError(t, err)

		// check GetSegment
		info0_0 := meta.GetHealthySegment(segID0_0)
		assert.NotNil(t, info0_0)
		assert.True(t, proto.Equal(info0_0, segInfo0_0))
		info1_0 := meta.GetHealthySegment(segID1_0)
		assert.NotNil(t, info1_0)
		assert.True(t, proto.Equal(info1_0, segInfo1_0))

		// check GetSegmentsOfCollection
		segIDs := meta.GetSegmentsIDOfCollection(collID)
		assert.EqualValues(t, 3, len(segIDs))
		assert.Contains(t, segIDs, segID0_0)
		assert.Contains(t, segIDs, segID1_0)
		assert.Contains(t, segIDs, segID1_1)

		// check GetSegmentsOfPartition
		segIDs = meta.GetSegmentsIDOfPartition(collID, partID0)
		assert.EqualValues(t, 1, len(segIDs))
		assert.Contains(t, segIDs, segID0_0)
		segIDs = meta.GetSegmentsIDOfPartition(collID, partID1)
		assert.EqualValues(t, 2, len(segIDs))
		assert.Contains(t, segIDs, segID1_0)
		assert.Contains(t, segIDs, segID1_1)

		// check DropSegment
		err = meta.DropSegment(segID1_0)
		assert.NoError(t, err)
		segIDs = meta.GetSegmentsIDOfPartition(collID, partID1)
		assert.EqualValues(t, 1, len(segIDs))
		assert.Contains(t, segIDs, segID1_1)

		err = meta.SetState(segID0_0, commonpb.SegmentState_Sealed)
		assert.NoError(t, err)
		err = meta.SetState(segID0_0, commonpb.SegmentState_Flushed)
		assert.NoError(t, err)

		info0_0 = meta.GetHealthySegment(segID0_0)
		assert.NotNil(t, info0_0)
		assert.EqualValues(t, commonpb.SegmentState_Flushed, info0_0.State)

		info0_0 = meta.GetHealthySegment(segID0_0)
		assert.NotNil(t, info0_0)
		assert.Equal(t, true, info0_0.GetIsImporting())
		err = meta.UnsetIsImporting(segID0_0)
		assert.NoError(t, err)
		info0_0 = meta.GetHealthySegment(segID0_0)
		assert.NotNil(t, info0_0)
		assert.Equal(t, false, info0_0.GetIsImporting())

		// UnsetIsImporting on segment that does not exist.
		err = meta.UnsetIsImporting(segID1_0)
		assert.Error(t, err)

		info1_1 := meta.GetHealthySegment(segID1_1)
		assert.NotNil(t, info1_1)
		assert.Equal(t, false, info1_1.GetIsImporting())
		err = meta.UnsetIsImporting(segID1_1)
		assert.NoError(t, err)
		info1_1 = meta.GetHealthySegment(segID1_1)
		assert.NotNil(t, info1_1)
		assert.Equal(t, false, info1_1.GetIsImporting())
	})

	t.Run("Test segment with kv fails", func(t *testing.T) {
		// inject error for `Save`
		metakv := mockkv.NewMetaKv(t)
		metakv.EXPECT().Save(mock.Anything, mock.Anything).Return(errors.New("failed")).Maybe()
		metakv.EXPECT().MultiSave(mock.Anything).Return(errors.New("failed")).Maybe()
		metakv.EXPECT().WalkWithPrefix(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
		metakv.EXPECT().LoadWithPrefix(mock.Anything).Return(nil, nil, nil).Maybe()
		catalog := datacoord.NewCatalog(metakv, "", "")
		meta, err := newMeta(context.TODO(), catalog, nil)
		assert.NoError(t, err)

		err = meta.AddSegment(context.TODO(), NewSegmentInfo(&datapb.SegmentInfo{}))
		assert.Error(t, err)

		metakv2 := mockkv.NewMetaKv(t)
		metakv2.EXPECT().Save(mock.Anything, mock.Anything).Return(nil).Maybe()
		metakv2.EXPECT().MultiSave(mock.Anything).Return(nil).Maybe()
		metakv2.EXPECT().Remove(mock.Anything).Return(errors.New("failed")).Maybe()
		metakv2.EXPECT().MultiRemove(mock.Anything).Return(errors.New("failed")).Maybe()
		metakv2.EXPECT().WalkWithPrefix(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
		metakv2.EXPECT().LoadWithPrefix(mock.Anything).Return(nil, nil, nil).Maybe()
		metakv2.EXPECT().MultiSaveAndRemoveWithPrefix(mock.Anything, mock.Anything).Return(errors.New("failed"))
		catalog = datacoord.NewCatalog(metakv2, "", "")
		meta, err = newMeta(context.TODO(), catalog, nil)
		assert.NoError(t, err)
		// nil, since no segment yet
		err = meta.DropSegment(0)
		assert.NoError(t, err)
		// nil, since Save error not injected
		err = meta.AddSegment(context.TODO(), NewSegmentInfo(&datapb.SegmentInfo{}))
		assert.NoError(t, err)
		// error injected
		err = meta.DropSegment(0)
		assert.Error(t, err)

		catalog = datacoord.NewCatalog(metakv, "", "")
		meta, err = newMeta(context.TODO(), catalog, nil)
		assert.NoError(t, err)
		assert.NotNil(t, meta)
	})

	t.Run("Test GetCount", func(t *testing.T) {
		const rowCount0 = 100
		const rowCount1 = 300

		// no segment
		nums := meta.GetNumRowsOfCollection(collID)
		assert.EqualValues(t, 0, nums)

		// add seg1 with 100 rows
		segID0, err := mockAllocator.allocID(ctx)
		assert.NoError(t, err)
		segInfo0 := buildSegment(collID, partID0, segID0, channelName, false)
		segInfo0.NumOfRows = rowCount0
		err = meta.AddSegment(context.TODO(), segInfo0)
		assert.NoError(t, err)

		// add seg2 with 300 rows
		segID1, err := mockAllocator.allocID(ctx)
		assert.NoError(t, err)
		segInfo1 := buildSegment(collID, partID0, segID1, channelName, false)
		segInfo1.NumOfRows = rowCount1
		err = meta.AddSegment(context.TODO(), segInfo1)
		assert.NoError(t, err)

		// check partition/collection statistics
		nums = meta.GetNumRowsOfPartition(collID, partID0)
		assert.EqualValues(t, (rowCount0 + rowCount1), nums)
		nums = meta.GetNumRowsOfCollection(collID)
		assert.EqualValues(t, (rowCount0 + rowCount1), nums)
	})

	t.Run("Test GetSegmentsChanPart", func(t *testing.T) {
		result := meta.GetSegmentsChanPart(func(*SegmentInfo) bool { return true })
		assert.Equal(t, 2, len(result))
		for _, entry := range result {
			assert.Equal(t, "c1", entry.channelName)
			if entry.partitionID == UniqueID(100) {
				assert.Equal(t, 3, len(entry.segments))
			}
			if entry.partitionID == UniqueID(101) {
				assert.Equal(t, 1, len(entry.segments))
			}
		}
		result = meta.GetSegmentsChanPart(func(seg *SegmentInfo) bool { return seg.GetCollectionID() == 10 })
		assert.Equal(t, 0, len(result))
	})

	t.Run("GetClonedCollectionInfo", func(t *testing.T) {
		// collection does not exist
		ret := meta.GetClonedCollectionInfo(-1)
		assert.Nil(t, ret)

		collInfo.Properties = map[string]string{
			common.CollectionTTLConfigKey: "3600",
		}
		meta.AddCollection(collInfo)
		ret = meta.GetClonedCollectionInfo(collInfo.ID)
		equalCollectionInfo(t, collInfo, ret)

		collInfo.StartPositions = []*commonpb.KeyDataPair{
			{
				Key:  "k",
				Data: []byte("v"),
			},
		}
		meta.AddCollection(collInfo)
		ret = meta.GetClonedCollectionInfo(collInfo.ID)
		equalCollectionInfo(t, collInfo, ret)
	})

	t.Run("Test GetCollectionBinlogSize", func(t *testing.T) {
		const size0 = 1024
		const size1 = 2048

		// add seg0 with size0
		segID0, err := mockAllocator.allocID(ctx)
		assert.NoError(t, err)
		segInfo0 := buildSegment(collID, partID0, segID0, channelName, false)
		segInfo0.size.Store(size0)
		err = meta.AddSegment(context.TODO(), segInfo0)
		assert.NoError(t, err)

		// add seg1 with size1
		segID1, err := mockAllocator.allocID(ctx)
		assert.NoError(t, err)
		segInfo1 := buildSegment(collID, partID0, segID1, channelName, false)
		segInfo1.size.Store(size1)
		err = meta.AddSegment(context.TODO(), segInfo1)
		assert.NoError(t, err)

		// check TotalBinlogSize
		total, collectionBinlogSize := meta.GetCollectionBinlogSize()
		assert.Len(t, collectionBinlogSize, 1)
		assert.Equal(t, int64(size0+size1), collectionBinlogSize[collID])
		assert.Equal(t, int64(size0+size1), total)
	})

	t.Run("Test AddAllocation", func(t *testing.T) {
		meta, _ := newMemoryMeta()
		err := meta.AddAllocation(1, &Allocation{
			SegmentID:  1,
			NumOfRows:  1,
			ExpireTime: 0,
		})
		assert.Error(t, err)
	})
}

func TestGetUnFlushedSegments(t *testing.T) {
	meta, err := newMemoryMeta()
	assert.NoError(t, err)
	s1 := &datapb.SegmentInfo{
		ID:           0,
		CollectionID: 0,
		PartitionID:  0,
		State:        commonpb.SegmentState_Growing,
	}
	err = meta.AddSegment(context.TODO(), NewSegmentInfo(s1))
	assert.NoError(t, err)
	s2 := &datapb.SegmentInfo{
		ID:           1,
		CollectionID: 0,
		PartitionID:  0,
		State:        commonpb.SegmentState_Flushed,
	}
	err = meta.AddSegment(context.TODO(), NewSegmentInfo(s2))
	assert.NoError(t, err)

	segments := meta.GetUnFlushedSegments()
	assert.NoError(t, err)

	assert.EqualValues(t, 1, len(segments))
	assert.EqualValues(t, 0, segments[0].ID)
	assert.NotEqualValues(t, commonpb.SegmentState_Flushed, segments[0].State)
}

func TestUpdateSegmentsInfo(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		meta, err := newMemoryMeta()
		assert.NoError(t, err)

		segment1 := &SegmentInfo{SegmentInfo: &datapb.SegmentInfo{
			ID: 1, State: commonpb.SegmentState_Growing,
			Binlogs:   []*datapb.FieldBinlog{getFieldBinlogIDs(1, 0)},
			Statslogs: []*datapb.FieldBinlog{getFieldBinlogIDs(1, 0)},
		}}
		err = meta.AddSegment(context.TODO(), segment1)
		assert.NoError(t, err)

		err = meta.UpdateSegmentsInfo(
			UpdateStatusOperator(1, commonpb.SegmentState_Flushing),
			UpdateBinlogsOperator(1,
				[]*datapb.FieldBinlog{getFieldBinlogIDsWithEntry(1, 10, 1)},
				[]*datapb.FieldBinlog{getFieldBinlogIDs(1, 1)},
				[]*datapb.FieldBinlog{{Binlogs: []*datapb.Binlog{{EntriesNum: 1, TimestampFrom: 100, TimestampTo: 200, LogSize: 1000, LogPath: getDeltaLogPath("deltalog1", 1)}}}},
			),
			UpdateStartPosition([]*datapb.SegmentStartPosition{{SegmentID: 1, StartPosition: &msgpb.MsgPosition{MsgID: []byte{1, 2, 3}}}}),
			UpdateCheckPointOperator(1, false, []*datapb.CheckPoint{{SegmentID: 1, NumOfRows: 10}}),
		)
		assert.NoError(t, err)

		updated := meta.GetHealthySegment(1)
		expected := &SegmentInfo{SegmentInfo: &datapb.SegmentInfo{
			ID: 1, State: commonpb.SegmentState_Flushing, NumOfRows: 10,
			StartPosition: &msgpb.MsgPosition{MsgID: []byte{1, 2, 3}},
			Binlogs:       []*datapb.FieldBinlog{getFieldBinlogIDs(1, 0, 1)},
			Statslogs:     []*datapb.FieldBinlog{getFieldBinlogIDs(1, 0, 1)},
			Deltalogs:     []*datapb.FieldBinlog{{Binlogs: []*datapb.Binlog{{EntriesNum: 1, TimestampFrom: 100, TimestampTo: 200, LogSize: 1000}}}},
		}}

		assert.Equal(t, updated.StartPosition, expected.StartPosition)
		assert.Equal(t, updated.DmlPosition, expected.DmlPosition)
		assert.Equal(t, updated.DmlPosition, expected.DmlPosition)
		assert.Equal(t, len(updated.Binlogs[0].Binlogs), len(expected.Binlogs[0].Binlogs))
		assert.Equal(t, len(updated.Statslogs[0].Binlogs), len(expected.Statslogs[0].Binlogs))
		assert.Equal(t, len(updated.Deltalogs[0].Binlogs), len(expected.Deltalogs[0].Binlogs))
		assert.Equal(t, updated.State, expected.State)
		assert.Equal(t, updated.size.Load(), expected.size.Load())
		assert.Equal(t, updated.NumOfRows, expected.NumOfRows)
	})

	t.Run("update compacted segment", func(t *testing.T) {
		meta, err := newMemoryMeta()
		assert.NoError(t, err)

		// segment not found
		err = meta.UpdateSegmentsInfo(
			UpdateCompactedOperator(1),
		)
		assert.NoError(t, err)

		// normal
		segment1 := &SegmentInfo{SegmentInfo: &datapb.SegmentInfo{
			ID: 1, State: commonpb.SegmentState_Flushed,
			Binlogs:   []*datapb.FieldBinlog{getFieldBinlogIDs(1, 0)},
			Statslogs: []*datapb.FieldBinlog{getFieldBinlogIDs(1, 0)},
		}}
		err = meta.AddSegment(context.TODO(), segment1)
		assert.NoError(t, err)

		err = meta.UpdateSegmentsInfo(
			UpdateCompactedOperator(1),
		)
		assert.NoError(t, err)
	})
	t.Run("update non-existed segment", func(t *testing.T) {
		meta, err := newMemoryMeta()
		assert.NoError(t, err)

		err = meta.UpdateSegmentsInfo(
			UpdateStatusOperator(1, commonpb.SegmentState_Flushing),
		)
		assert.NoError(t, err)

		err = meta.UpdateSegmentsInfo(
			UpdateBinlogsOperator(1, nil, nil, nil),
		)
		assert.NoError(t, err)

		err = meta.UpdateSegmentsInfo(
			UpdateStartPosition([]*datapb.SegmentStartPosition{{SegmentID: 1, StartPosition: &msgpb.MsgPosition{MsgID: []byte{1, 2, 3}}}}),
		)
		assert.NoError(t, err)

		err = meta.UpdateSegmentsInfo(
			UpdateCheckPointOperator(1, false, []*datapb.CheckPoint{{SegmentID: 1, NumOfRows: 10}}),
		)
		assert.NoError(t, err)
	})

	t.Run("update checkpoints and start position of non existed segment", func(t *testing.T) {
		meta, err := newMemoryMeta()
		assert.NoError(t, err)

		segment1 := &SegmentInfo{SegmentInfo: &datapb.SegmentInfo{ID: 1, State: commonpb.SegmentState_Growing}}
		err = meta.AddSegment(context.TODO(), segment1)
		assert.NoError(t, err)

		err = meta.UpdateSegmentsInfo(
			UpdateCheckPointOperator(1, false, []*datapb.CheckPoint{{SegmentID: 2, NumOfRows: 10}}),
		)

		assert.NoError(t, err)
		assert.Nil(t, meta.GetHealthySegment(2))
	})

	t.Run("test save etcd failed", func(t *testing.T) {
		metakv := mockkv.NewMetaKv(t)
		metakv.EXPECT().Save(mock.Anything, mock.Anything).Return(errors.New("mocked fail")).Maybe()
		metakv.EXPECT().MultiSave(mock.Anything).Return(errors.New("mocked fail")).Maybe()
		metakv.EXPECT().WalkWithPrefix(mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
		metakv.EXPECT().LoadWithPrefix(mock.Anything).Return(nil, nil, nil).Maybe()
		catalog := datacoord.NewCatalog(metakv, "", "")
		meta, err := newMeta(context.TODO(), catalog, nil)
		assert.NoError(t, err)

		segmentInfo := &SegmentInfo{
			SegmentInfo: &datapb.SegmentInfo{
				ID:        1,
				NumOfRows: 0,
				State:     commonpb.SegmentState_Growing,
			},
		}
		meta.segments.SetSegment(1, segmentInfo)

		err = meta.UpdateSegmentsInfo(
			UpdateStatusOperator(1, commonpb.SegmentState_Flushing),
			UpdateBinlogsOperator(1,
				[]*datapb.FieldBinlog{getFieldBinlogIDs(1, 0)},
				[]*datapb.FieldBinlog{getFieldBinlogIDs(1, 0)},
				[]*datapb.FieldBinlog{{Binlogs: []*datapb.Binlog{{EntriesNum: 1, TimestampFrom: 100, TimestampTo: 200, LogSize: 1000, LogPath: getDeltaLogPath("deltalog", 1)}}}},
			),
			UpdateStartPosition([]*datapb.SegmentStartPosition{{SegmentID: 1, StartPosition: &msgpb.MsgPosition{MsgID: []byte{1, 2, 3}}}}),
			UpdateCheckPointOperator(1, false, []*datapb.CheckPoint{{SegmentID: 1, NumOfRows: 10}}),
		)

		assert.Error(t, err)
		assert.Equal(t, "mocked fail", err.Error())
		segmentInfo = meta.GetHealthySegment(1)
		assert.EqualValues(t, 0, segmentInfo.NumOfRows)
		assert.Equal(t, commonpb.SegmentState_Growing, segmentInfo.State)
		assert.Nil(t, segmentInfo.Binlogs)
		assert.Nil(t, segmentInfo.StartPosition)
	})
}

func Test_meta_SetSegmentCompacting(t *testing.T) {
	type fields struct {
		client   kv.MetaKv
		segments *SegmentsInfo
	}
	type args struct {
		segmentID  UniqueID
		compacting bool
	}
	tests := []struct {
		name   string
		fields fields
		args   args
	}{
		{
			"test set segment compacting",
			fields{
				NewMetaMemoryKV(),
				&SegmentsInfo{
					map[int64]*SegmentInfo{
						1: {
							SegmentInfo: &datapb.SegmentInfo{
								ID:    1,
								State: commonpb.SegmentState_Flushed,
							},
							isCompacting: false,
						},
					},
				},
			},
			args{
				segmentID:  1,
				compacting: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &meta{
				catalog:  &datacoord.Catalog{MetaKv: tt.fields.client},
				segments: tt.fields.segments,
			}
			m.SetSegmentCompacting(tt.args.segmentID, tt.args.compacting)
			segment := m.GetHealthySegment(tt.args.segmentID)
			assert.Equal(t, tt.args.compacting, segment.isCompacting)
		})
	}
}

func Test_meta_SetSegmentImporting(t *testing.T) {
	type fields struct {
		client   kv.MetaKv
		segments *SegmentsInfo
	}
	type args struct {
		segmentID UniqueID
		importing bool
	}
	tests := []struct {
		name   string
		fields fields
		args   args
	}{
		{
			"test set segment importing",
			fields{
				NewMetaMemoryKV(),
				&SegmentsInfo{
					map[int64]*SegmentInfo{
						1: {
							SegmentInfo: &datapb.SegmentInfo{
								ID:          1,
								State:       commonpb.SegmentState_Flushed,
								IsImporting: false,
							},
						},
					},
				},
			},
			args{
				segmentID: 1,
				importing: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &meta{
				catalog:  &datacoord.Catalog{MetaKv: tt.fields.client},
				segments: tt.fields.segments,
			}
			m.SetSegmentCompacting(tt.args.segmentID, tt.args.importing)
			segment := m.GetHealthySegment(tt.args.segmentID)
			assert.Equal(t, tt.args.importing, segment.isCompacting)
		})
	}
}

func Test_meta_GetSegmentsOfCollection(t *testing.T) {
	storedSegments := &SegmentsInfo{
		map[int64]*SegmentInfo{
			1: {
				SegmentInfo: &datapb.SegmentInfo{
					ID:           1,
					CollectionID: 1,
					State:        commonpb.SegmentState_Flushed,
				},
			},
			2: {
				SegmentInfo: &datapb.SegmentInfo{
					ID:           2,
					CollectionID: 1,
					State:        commonpb.SegmentState_Growing,
				},
			},
			3: {
				SegmentInfo: &datapb.SegmentInfo{
					ID:           3,
					CollectionID: 2,
					State:        commonpb.SegmentState_Flushed,
				},
			},
		},
	}
	expectedSeg := map[int64]commonpb.SegmentState{1: commonpb.SegmentState_Flushed, 2: commonpb.SegmentState_Growing}
	m := &meta{segments: storedSegments}
	got := m.GetSegmentsOfCollection(1)
	assert.Equal(t, len(expectedSeg), len(got))
	for _, gotInfo := range got {
		expected, ok := expectedSeg[gotInfo.ID]
		assert.True(t, ok)
		assert.Equal(t, expected, gotInfo.GetState())
	}
}

func TestMeta_HasSegments(t *testing.T) {
	m := &meta{
		segments: &SegmentsInfo{
			segments: map[UniqueID]*SegmentInfo{
				1: {
					SegmentInfo: &datapb.SegmentInfo{
						ID: 1,
					},
					currRows: 100,
				},
			},
		},
	}

	has, err := m.HasSegments([]UniqueID{1})
	assert.Equal(t, true, has)
	assert.NoError(t, err)

	has, err = m.HasSegments([]UniqueID{2})
	assert.Equal(t, false, has)
	assert.Error(t, err)
}

func TestMeta_GetAllSegments(t *testing.T) {
	m := &meta{
		segments: &SegmentsInfo{
			segments: map[UniqueID]*SegmentInfo{
				1: {
					SegmentInfo: &datapb.SegmentInfo{
						ID:    1,
						State: commonpb.SegmentState_Growing,
					},
				},
				2: {
					SegmentInfo: &datapb.SegmentInfo{
						ID:    2,
						State: commonpb.SegmentState_Dropped,
					},
				},
			},
		},
	}

	seg1 := m.GetHealthySegment(1)
	seg1All := m.GetSegment(1)
	seg2 := m.GetHealthySegment(2)
	seg2All := m.GetSegment(2)
	assert.NotNil(t, seg1)
	assert.NotNil(t, seg1All)
	assert.Nil(t, seg2)
	assert.NotNil(t, seg2All)
}

func TestMeta_isSegmentHealthy_issue17823_panic(t *testing.T) {
	var seg *SegmentInfo

	assert.False(t, isSegmentHealthy(seg))
}

func equalCollectionInfo(t *testing.T, a *collectionInfo, b *collectionInfo) {
	assert.Equal(t, a.ID, b.ID)
	assert.Equal(t, a.Partitions, b.Partitions)
	assert.Equal(t, a.Schema, b.Schema)
	assert.Equal(t, a.Properties, b.Properties)
	assert.Equal(t, a.StartPositions, b.StartPositions)
}

func TestChannelCP(t *testing.T) {
	mockVChannel := "fake-by-dev-rootcoord-dml-1-testchannelcp-v0"
	mockPChannel := "fake-by-dev-rootcoord-dml-1"

	pos := &msgpb.MsgPosition{
		ChannelName: mockPChannel,
		MsgID:       []byte{0, 0, 0, 0, 0, 0, 0, 0},
		Timestamp:   1000,
	}

	t.Run("UpdateChannelCheckpoint", func(t *testing.T) {
		meta, err := newMemoryMeta()
		assert.NoError(t, err)

		// nil position
		err = meta.UpdateChannelCheckpoint(mockVChannel, nil)
		assert.Error(t, err)

		err = meta.UpdateChannelCheckpoint(mockVChannel, pos)
		assert.NoError(t, err)
	})

	t.Run("GetChannelCheckpoint", func(t *testing.T) {
		meta, err := newMemoryMeta()
		assert.NoError(t, err)

		position := meta.GetChannelCheckpoint(mockVChannel)
		assert.Nil(t, position)

		err = meta.UpdateChannelCheckpoint(mockVChannel, pos)
		assert.NoError(t, err)
		position = meta.GetChannelCheckpoint(mockVChannel)
		assert.NotNil(t, position)
		assert.True(t, position.ChannelName == pos.ChannelName)
		assert.True(t, position.Timestamp == pos.Timestamp)
	})

	t.Run("DropChannelCheckpoint", func(t *testing.T) {
		meta, err := newMemoryMeta()
		assert.NoError(t, err)

		err = meta.DropChannelCheckpoint(mockVChannel)
		assert.NoError(t, err)

		err = meta.UpdateChannelCheckpoint(mockVChannel, pos)
		assert.NoError(t, err)
		err = meta.DropChannelCheckpoint(mockVChannel)
		assert.NoError(t, err)
	})
}

func Test_meta_GcConfirm(t *testing.T) {
	m := &meta{}
	catalog := mocks.NewDataCoordCatalog(t)
	m.catalog = catalog

	catalog.On("GcConfirm",
		mock.Anything,
		mock.AnythingOfType("int64"),
		mock.AnythingOfType("int64")).
		Return(false)

	assert.False(t, m.GcConfirm(context.TODO(), 100, 10000))
}
