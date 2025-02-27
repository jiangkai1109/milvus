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

package segments

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/apache/arrow/go/v12/arrow"
	"github.com/apache/arrow/go/v12/arrow/array"
	"github.com/apache/arrow/go/v12/arrow/memory"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	milvus_storage "github.com/milvus-io/milvus-storage/go/storage"
	"github.com/milvus-io/milvus-storage/go/storage/options"
	"github.com/milvus-io/milvus-storage/go/storage/schema"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/util/initcore"
	"github.com/milvus-io/milvus/internal/util/typeutil"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/util/funcutil"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/metric"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
)

type SegmentLoaderSuite struct {
	suite.Suite
	loader Loader

	// Dependencies
	manager      *Manager
	rootPath     string
	chunkManager storage.ChunkManager

	// Data
	collectionID int64
	partitionID  int64
	segmentID    int64
	schema       *schemapb.CollectionSchema
	segmentNum   int
}

func (suite *SegmentLoaderSuite) SetupSuite() {
	paramtable.Init()
	suite.rootPath = suite.T().Name()
	suite.collectionID = rand.Int63()
	suite.partitionID = rand.Int63()
	suite.segmentID = rand.Int63()
	suite.segmentNum = 5
}

func (suite *SegmentLoaderSuite) SetupTest() {
	// Dependencies
	suite.manager = NewManager()
	ctx := context.Background()
	// TODO:: cpp chunk manager not support local chunk manager
	// suite.chunkManager = storage.NewLocalChunkManager(storage.RootPath(
	//	fmt.Sprintf("/tmp/milvus-ut/%d", rand.Int63())))
	chunkManagerFactory := NewTestChunkManagerFactory(paramtable.Get(), suite.rootPath)
	suite.chunkManager, _ = chunkManagerFactory.NewPersistentStorageChunkManager(ctx)
	suite.loader = NewLoader(suite.manager, suite.chunkManager)
	initcore.InitRemoteChunkManager(paramtable.Get())

	// Data
	suite.schema = GenTestCollectionSchema("test", schemapb.DataType_Int64)
	indexMeta := GenTestIndexMeta(suite.collectionID, suite.schema)
	loadMeta := &querypb.LoadMetaInfo{
		LoadType:     querypb.LoadType_LoadCollection,
		CollectionID: suite.collectionID,
		PartitionIDs: []int64{suite.partitionID},
	}
	suite.manager.Collection.PutOrRef(suite.collectionID, suite.schema, indexMeta, loadMeta)
}

func (suite *SegmentLoaderSuite) TearDownTest() {
	ctx := context.Background()
	for i := 0; i < suite.segmentNum; i++ {
		suite.manager.Segment.Remove(suite.segmentID+int64(i), querypb.DataScope_All)
	}
	suite.chunkManager.RemoveWithPrefix(ctx, suite.rootPath)
}

func (suite *SegmentLoaderSuite) TestLoad() {
	ctx := context.Background()

	msgLength := 4

	// Load sealed
	binlogs, statsLogs, err := SaveBinLog(ctx,
		suite.collectionID,
		suite.partitionID,
		suite.segmentID,
		msgLength,
		suite.schema,
		suite.chunkManager,
	)
	suite.NoError(err)

	_, err = suite.loader.Load(ctx, suite.collectionID, SegmentTypeSealed, 0, &querypb.SegmentLoadInfo{
		SegmentID:    suite.segmentID,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		BinlogPaths:  binlogs,
		Statslogs:    statsLogs,
		NumOfRows:    int64(msgLength),
	})
	suite.NoError(err)

	// Load growing
	binlogs, statsLogs, err = SaveBinLog(ctx,
		suite.collectionID,
		suite.partitionID,
		suite.segmentID+1,
		msgLength,
		suite.schema,
		suite.chunkManager,
	)
	suite.NoError(err)

	_, err = suite.loader.Load(ctx, suite.collectionID, SegmentTypeGrowing, 0, &querypb.SegmentLoadInfo{
		SegmentID:    suite.segmentID + 1,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		BinlogPaths:  binlogs,
		Statslogs:    statsLogs,
		NumOfRows:    int64(msgLength),
	})
	suite.NoError(err)
}

func (suite *SegmentLoaderSuite) TestLoadFail() {
	ctx := context.Background()

	msgLength := 4

	// Load sealed
	binlogs, statsLogs, err := SaveBinLog(ctx,
		suite.collectionID,
		suite.partitionID,
		suite.segmentID,
		msgLength,
		suite.schema,
		suite.chunkManager,
	)
	suite.NoError(err)

	// make file & binlog mismatch
	for _, binlog := range binlogs {
		for _, log := range binlog.GetBinlogs() {
			log.LogPath = log.LogPath + "-suffix"
		}
	}

	_, err = suite.loader.Load(ctx, suite.collectionID, SegmentTypeSealed, 0, &querypb.SegmentLoadInfo{
		SegmentID:    suite.segmentID,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		BinlogPaths:  binlogs,
		Statslogs:    statsLogs,
		NumOfRows:    int64(msgLength),
	})
	suite.Error(err)
}

func (suite *SegmentLoaderSuite) TestLoadMultipleSegments() {
	ctx := context.Background()
	loadInfos := make([]*querypb.SegmentLoadInfo, 0, suite.segmentNum)

	msgLength := 100
	// Load sealed
	for i := 0; i < suite.segmentNum; i++ {
		segmentID := suite.segmentID + int64(i)
		binlogs, statsLogs, err := SaveBinLog(ctx,
			suite.collectionID,
			suite.partitionID,
			segmentID,
			msgLength,
			suite.schema,
			suite.chunkManager,
		)
		suite.NoError(err)
		loadInfos = append(loadInfos, &querypb.SegmentLoadInfo{
			SegmentID:    segmentID,
			PartitionID:  suite.partitionID,
			CollectionID: suite.collectionID,
			BinlogPaths:  binlogs,
			Statslogs:    statsLogs,
			NumOfRows:    int64(msgLength),
		})
	}

	segments, err := suite.loader.Load(ctx, suite.collectionID, SegmentTypeSealed, 0, loadInfos...)
	suite.NoError(err)

	// Won't load bloom filter with sealed segments
	for _, segment := range segments {
		for pk := 0; pk < 100; pk++ {
			exist := segment.MayPkExist(storage.NewInt64PrimaryKey(int64(pk)))
			suite.Require().False(exist)
		}
	}

	// Load growing
	loadInfos = loadInfos[:0]
	for i := 0; i < suite.segmentNum; i++ {
		segmentID := suite.segmentID + int64(suite.segmentNum) + int64(i)
		binlogs, statsLogs, err := SaveBinLog(ctx,
			suite.collectionID,
			suite.partitionID,
			segmentID,
			msgLength,
			suite.schema,
			suite.chunkManager,
		)
		suite.NoError(err)
		loadInfos = append(loadInfos, &querypb.SegmentLoadInfo{
			SegmentID:    segmentID,
			PartitionID:  suite.partitionID,
			CollectionID: suite.collectionID,
			BinlogPaths:  binlogs,
			Statslogs:    statsLogs,
			NumOfRows:    int64(msgLength),
		})
	}

	segments, err = suite.loader.Load(ctx, suite.collectionID, SegmentTypeGrowing, 0, loadInfos...)
	suite.NoError(err)
	// Should load bloom filter with growing segments
	for _, segment := range segments {
		for pk := 0; pk < 100; pk++ {
			exist := segment.MayPkExist(storage.NewInt64PrimaryKey(int64(pk)))
			suite.True(exist)
		}
	}
}

func (suite *SegmentLoaderSuite) TestLoadWithIndex() {
	ctx := context.Background()
	loadInfos := make([]*querypb.SegmentLoadInfo, 0, suite.segmentNum)

	msgLength := 100
	// Load sealed
	for i := 0; i < suite.segmentNum; i++ {
		segmentID := suite.segmentID + int64(i)
		binlogs, statsLogs, err := SaveBinLog(ctx,
			suite.collectionID,
			suite.partitionID,
			segmentID,
			msgLength,
			suite.schema,
			suite.chunkManager,
		)
		suite.NoError(err)

		vecFields := funcutil.GetVecFieldIDs(suite.schema)
		indexInfo, err := GenAndSaveIndex(
			suite.collectionID,
			suite.partitionID,
			segmentID,
			vecFields[0],
			msgLength,
			IndexFaissIVFFlat,
			metric.L2,
			suite.chunkManager,
		)
		suite.NoError(err)
		loadInfos = append(loadInfos, &querypb.SegmentLoadInfo{
			SegmentID:    segmentID,
			PartitionID:  suite.partitionID,
			CollectionID: suite.collectionID,
			BinlogPaths:  binlogs,
			Statslogs:    statsLogs,
			IndexInfos:   []*querypb.FieldIndexInfo{indexInfo},
			NumOfRows:    int64(msgLength),
		})
	}

	segments, err := suite.loader.Load(ctx, suite.collectionID, SegmentTypeSealed, 0, loadInfos...)
	suite.NoError(err)

	vecFields := funcutil.GetVecFieldIDs(suite.schema)
	for _, segment := range segments {
		suite.True(segment.ExistIndex(vecFields[0]))
	}
}

func (suite *SegmentLoaderSuite) TestLoadBloomFilter() {
	ctx := context.Background()
	loadInfos := make([]*querypb.SegmentLoadInfo, 0, suite.segmentNum)

	msgLength := 100
	// Load sealed
	for i := 0; i < suite.segmentNum; i++ {
		segmentID := suite.segmentID + int64(i)
		binlogs, statsLogs, err := SaveBinLog(ctx,
			suite.collectionID,
			suite.partitionID,
			segmentID,
			msgLength,
			suite.schema,
			suite.chunkManager,
		)
		suite.NoError(err)

		loadInfos = append(loadInfos, &querypb.SegmentLoadInfo{
			SegmentID:    segmentID,
			PartitionID:  suite.partitionID,
			CollectionID: suite.collectionID,
			BinlogPaths:  binlogs,
			Statslogs:    statsLogs,
			NumOfRows:    int64(msgLength),
		})
	}

	bfs, err := suite.loader.LoadBloomFilterSet(ctx, suite.collectionID, 0, loadInfos...)
	suite.NoError(err)

	for _, bf := range bfs {
		for pk := 0; pk < 100; pk++ {
			exist := bf.MayPkExist(storage.NewInt64PrimaryKey(int64(pk)))
			suite.Require().True(exist)
		}
	}
}

func (suite *SegmentLoaderSuite) TestLoadDeltaLogs() {
	ctx := context.Background()
	loadInfos := make([]*querypb.SegmentLoadInfo, 0, suite.segmentNum)

	msgLength := 100
	// Load sealed
	for i := 0; i < suite.segmentNum; i++ {
		segmentID := suite.segmentID + int64(i)
		binlogs, statsLogs, err := SaveBinLog(ctx,
			suite.collectionID,
			suite.partitionID,
			segmentID,
			msgLength,
			suite.schema,
			suite.chunkManager,
		)
		suite.NoError(err)

		// Delete PKs 1, 2
		deltaLogs, err := SaveDeltaLog(suite.collectionID,
			suite.partitionID,
			segmentID,
			suite.chunkManager,
		)
		suite.NoError(err)

		loadInfos = append(loadInfos, &querypb.SegmentLoadInfo{
			SegmentID:    segmentID,
			PartitionID:  suite.partitionID,
			CollectionID: suite.collectionID,
			BinlogPaths:  binlogs,
			Statslogs:    statsLogs,
			Deltalogs:    deltaLogs,
			NumOfRows:    int64(msgLength),
		})
	}

	segments, err := suite.loader.Load(ctx, suite.collectionID, SegmentTypeGrowing, 0, loadInfos...)
	suite.NoError(err)

	for _, segment := range segments {
		suite.Equal(int64(100-2), segment.RowNum())
		for pk := 0; pk < 100; pk++ {
			if pk == 1 || pk == 2 {
				continue
			}
			exist := segment.MayPkExist(storage.NewInt64PrimaryKey(int64(pk)))
			suite.Require().True(exist)
		}
	}
}

func (suite *SegmentLoaderSuite) TestLoadDupDeltaLogs() {
	ctx := context.Background()
	loadInfos := make([]*querypb.SegmentLoadInfo, 0, suite.segmentNum)

	msgLength := 100
	// Load sealed
	for i := 0; i < suite.segmentNum; i++ {
		segmentID := suite.segmentID + int64(i)
		binlogs, statsLogs, err := SaveBinLog(ctx,
			suite.collectionID,
			suite.partitionID,
			segmentID,
			msgLength,
			suite.schema,
			suite.chunkManager,
		)
		suite.NoError(err)

		// Delete PKs 1, 2
		deltaLogs, err := SaveDeltaLog(suite.collectionID,
			suite.partitionID,
			segmentID,
			suite.chunkManager,
		)
		suite.NoError(err)

		loadInfos = append(loadInfos, &querypb.SegmentLoadInfo{
			SegmentID:    segmentID,
			PartitionID:  suite.partitionID,
			CollectionID: suite.collectionID,
			BinlogPaths:  binlogs,
			Statslogs:    statsLogs,
			Deltalogs:    deltaLogs,
			NumOfRows:    int64(msgLength),
		})
	}

	segments, err := suite.loader.Load(ctx, suite.collectionID, SegmentTypeGrowing, 0, loadInfos...)
	suite.NoError(err)

	for i, segment := range segments {
		suite.Equal(int64(100-2), segment.RowNum())
		for pk := 0; pk < 100; pk++ {
			if pk == 1 || pk == 2 {
				continue
			}
			exist := segment.MayPkExist(storage.NewInt64PrimaryKey(int64(pk)))
			suite.Require().True(exist)
		}

		seg := segment.(*LocalSegment)
		// nothing would happen as the delta logs have been all applied,
		// so the released segment won't cause error
		seg.Release()
		loadInfos[i].Deltalogs[0].Binlogs[0].TimestampTo--
		err := suite.loader.LoadDeltaLogs(ctx, seg, loadInfos[i].GetDeltalogs())
		suite.NoError(err)
	}
}

func (suite *SegmentLoaderSuite) TestLoadIndex() {
	ctx := context.Background()
	segment := &LocalSegment{}
	loadInfo := &querypb.SegmentLoadInfo{
		SegmentID:    1,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		IndexInfos: []*querypb.FieldIndexInfo{
			{
				IndexFilePaths: []string{},
			},
		},
	}

	err := suite.loader.LoadIndex(ctx, segment, loadInfo, 0)
	suite.ErrorIs(err, merr.ErrIndexNotFound)
}

func (suite *SegmentLoaderSuite) TestLoadWithMmap() {
	key := paramtable.Get().QueryNodeCfg.MmapDirPath.Key
	paramtable.Get().Save(key, "/tmp/mmap-test")
	defer paramtable.Get().Reset(key)
	ctx := context.Background()

	collection := suite.manager.Collection.Get(suite.collectionID)
	for _, field := range collection.Schema().GetFields() {
		field.TypeParams = append(field.TypeParams, &commonpb.KeyValuePair{
			Key:   common.MmapEnabledKey,
			Value: "true",
		})
	}

	msgLength := 100
	// Load sealed
	binlogs, statsLogs, err := SaveBinLog(ctx,
		suite.collectionID,
		suite.partitionID,
		suite.segmentID,
		msgLength,
		suite.schema,
		suite.chunkManager,
	)
	suite.NoError(err)

	_, err = suite.loader.Load(ctx, suite.collectionID, SegmentTypeSealed, 0, &querypb.SegmentLoadInfo{
		SegmentID:    suite.segmentID,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		BinlogPaths:  binlogs,
		Statslogs:    statsLogs,
		NumOfRows:    int64(msgLength),
	})
	suite.NoError(err)
}

func (suite *SegmentLoaderSuite) TestPatchEntryNum() {
	ctx := context.Background()

	msgLength := 100
	segmentID := suite.segmentID
	binlogs, statsLogs, err := SaveBinLog(ctx,
		suite.collectionID,
		suite.partitionID,
		segmentID,
		msgLength,
		suite.schema,
		suite.chunkManager,
	)
	suite.NoError(err)

	vecFields := funcutil.GetVecFieldIDs(suite.schema)
	indexInfo, err := GenAndSaveIndex(
		suite.collectionID,
		suite.partitionID,
		segmentID,
		vecFields[0],
		msgLength,
		IndexFaissIVFFlat,
		metric.L2,
		suite.chunkManager,
	)
	suite.NoError(err)
	loadInfo := &querypb.SegmentLoadInfo{
		SegmentID:    segmentID,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		BinlogPaths:  binlogs,
		Statslogs:    statsLogs,
		IndexInfos:   []*querypb.FieldIndexInfo{indexInfo},
		NumOfRows:    int64(msgLength),
	}

	// mock legacy binlog entry num is zero case
	for _, fieldLog := range binlogs {
		for _, binlog := range fieldLog.GetBinlogs() {
			binlog.EntriesNum = 0
		}
	}

	segments, err := suite.loader.Load(ctx, suite.collectionID, SegmentTypeSealed, 0, loadInfo)
	suite.Require().NoError(err)
	suite.Require().Equal(1, len(segments))

	segment := segments[0]
	info := segment.GetIndex(vecFields[0])
	suite.Require().NotNil(info)

	for _, binlog := range info.FieldBinlog.GetBinlogs() {
		suite.Greater(binlog.EntriesNum, int64(0))
	}
}

func (suite *SegmentLoaderSuite) TestRunOutMemory() {
	ctx := context.Background()
	paramtable.Get().Save(paramtable.Get().QueryNodeCfg.OverloadedMemoryThresholdPercentage.Key, "0")
	defer paramtable.Get().Reset(paramtable.Get().QueryNodeCfg.OverloadedMemoryThresholdPercentage.Key)

	msgLength := 4

	// Load sealed
	binlogs, statsLogs, err := SaveBinLog(ctx,
		suite.collectionID,
		suite.partitionID,
		suite.segmentID,
		msgLength,
		suite.schema,
		suite.chunkManager,
	)
	suite.NoError(err)

	_, err = suite.loader.Load(ctx, suite.collectionID, SegmentTypeSealed, 0, &querypb.SegmentLoadInfo{
		SegmentID:    suite.segmentID,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		BinlogPaths:  binlogs,
		Statslogs:    statsLogs,
		NumOfRows:    int64(msgLength),
	})
	suite.Error(err)

	// Load growing
	binlogs, statsLogs, err = SaveBinLog(ctx,
		suite.collectionID,
		suite.partitionID,
		suite.segmentID+1,
		msgLength,
		suite.schema,
		suite.chunkManager,
	)
	suite.NoError(err)

	_, err = suite.loader.Load(ctx, suite.collectionID, SegmentTypeGrowing, 0, &querypb.SegmentLoadInfo{
		SegmentID:    suite.segmentID + 1,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		BinlogPaths:  binlogs,
		Statslogs:    statsLogs,
		NumOfRows:    int64(msgLength),
	})
	suite.Error(err)

	paramtable.Get().Save(paramtable.Get().QueryNodeCfg.MmapDirPath.Key, "./mmap")
	_, err = suite.loader.Load(ctx, suite.collectionID, SegmentTypeSealed, 0, &querypb.SegmentLoadInfo{
		SegmentID:    suite.segmentID,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		BinlogPaths:  binlogs,
		Statslogs:    statsLogs,
		NumOfRows:    int64(msgLength),
	})
	suite.Error(err)
	_, err = suite.loader.Load(ctx, suite.collectionID, SegmentTypeGrowing, 0, &querypb.SegmentLoadInfo{
		SegmentID:    suite.segmentID + 1,
		PartitionID:  suite.partitionID,
		CollectionID: suite.collectionID,
		BinlogPaths:  binlogs,
		Statslogs:    statsLogs,
		NumOfRows:    int64(msgLength),
	})
	suite.Error(err)
}

type SegmentLoaderDetailSuite struct {
	suite.Suite

	loader            *segmentLoader
	manager           *Manager
	segmentManager    *MockSegmentManager
	collectionManager *MockCollectionManager

	rootPath     string
	chunkManager storage.ChunkManager

	// Data
	collectionID int64
	partitionID  int64
	segmentID    int64
	schema       *schemapb.CollectionSchema
	segmentNum   int
}

func (suite *SegmentLoaderDetailSuite) SetupSuite() {
	paramtable.Init()
	suite.rootPath = suite.T().Name()
	suite.collectionID = rand.Int63()
	suite.partitionID = rand.Int63()
	suite.segmentID = rand.Int63()
	suite.segmentNum = 5
	suite.schema = GenTestCollectionSchema("test", schemapb.DataType_Int64)
}

func (suite *SegmentLoaderDetailSuite) SetupTest() {
	// Dependencies
	suite.collectionManager = NewMockCollectionManager(suite.T())
	suite.segmentManager = NewMockSegmentManager(suite.T())
	suite.manager = &Manager{
		Segment:    suite.segmentManager,
		Collection: suite.collectionManager,
	}

	ctx := context.Background()
	chunkManagerFactory := NewTestChunkManagerFactory(paramtable.Get(), suite.rootPath)
	suite.chunkManager, _ = chunkManagerFactory.NewPersistentStorageChunkManager(ctx)
	suite.loader = NewLoader(suite.manager, suite.chunkManager)
	initcore.InitRemoteChunkManager(paramtable.Get())

	// Data
	schema := GenTestCollectionSchema("test", schemapb.DataType_Int64)

	indexMeta := GenTestIndexMeta(suite.collectionID, schema)
	loadMeta := &querypb.LoadMetaInfo{
		LoadType:     querypb.LoadType_LoadCollection,
		CollectionID: suite.collectionID,
		PartitionIDs: []int64{suite.partitionID},
	}

	collection := NewCollection(suite.collectionID, schema, indexMeta, loadMeta.GetLoadType())
	suite.collectionManager.EXPECT().Get(suite.collectionID).Return(collection).Maybe()
}

func (suite *SegmentLoaderDetailSuite) TestWaitSegmentLoadDone() {
	suite.Run("wait_success", func() {
		idx := 0

		var infos []*querypb.SegmentLoadInfo
		suite.segmentManager.EXPECT().GetBy(mock.Anything, mock.Anything).Return(nil)
		suite.segmentManager.EXPECT().GetWithType(suite.segmentID, SegmentTypeSealed).RunAndReturn(func(segmentID int64, segmentType commonpb.SegmentState) Segment {
			defer func() { idx++ }()
			if idx == 0 {
				go func() {
					<-time.After(time.Second)
					suite.loader.notifyLoadFinish(infos...)
				}()
			}
			return nil
		})
		suite.segmentManager.EXPECT().UpdateBy(mock.Anything, mock.Anything, mock.Anything).Return(0)
		infos = suite.loader.prepare(SegmentTypeSealed, &querypb.SegmentLoadInfo{
			SegmentID:    suite.segmentID,
			PartitionID:  suite.partitionID,
			CollectionID: suite.collectionID,
			NumOfRows:    100,
		})

		err := suite.loader.waitSegmentLoadDone(context.Background(), SegmentTypeSealed, []int64{suite.segmentID}, 0)
		suite.NoError(err)
	})

	suite.Run("wait_failure", func() {
		suite.SetupTest()

		var idx int
		var infos []*querypb.SegmentLoadInfo
		suite.segmentManager.EXPECT().GetBy(mock.Anything, mock.Anything).Return(nil)
		suite.segmentManager.EXPECT().GetWithType(suite.segmentID, SegmentTypeSealed).RunAndReturn(func(segmentID int64, segmentType commonpb.SegmentState) Segment {
			defer func() { idx++ }()
			if idx == 0 {
				go func() {
					<-time.After(time.Second)
					suite.loader.unregister(infos...)
				}()
			}

			return nil
		})
		infos = suite.loader.prepare(SegmentTypeSealed, &querypb.SegmentLoadInfo{
			SegmentID:    suite.segmentID,
			PartitionID:  suite.partitionID,
			CollectionID: suite.collectionID,
			NumOfRows:    100,
		})

		err := suite.loader.waitSegmentLoadDone(context.Background(), SegmentTypeSealed, []int64{suite.segmentID}, 0)
		suite.Error(err)
	})

	suite.Run("wait_timeout", func() {
		suite.SetupTest()

		suite.segmentManager.EXPECT().GetBy(mock.Anything, mock.Anything).Return(nil)
		suite.segmentManager.EXPECT().GetWithType(suite.segmentID, SegmentTypeSealed).RunAndReturn(func(segmentID int64, segmentType commonpb.SegmentState) Segment {
			return nil
		})
		suite.loader.prepare(SegmentTypeSealed, &querypb.SegmentLoadInfo{
			SegmentID:    suite.segmentID,
			PartitionID:  suite.partitionID,
			CollectionID: suite.collectionID,
			NumOfRows:    100,
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := suite.loader.waitSegmentLoadDone(ctx, SegmentTypeSealed, []int64{suite.segmentID}, 0)
		suite.Error(err)
		suite.True(merr.IsCanceledOrTimeout(err))
	})
}

func (suite *SegmentLoaderDetailSuite) TestRequestResource() {
	suite.Run("out_of_memory_zero_info", func() {
		paramtable.Get().Save(paramtable.Get().QueryNodeCfg.OverloadedMemoryThresholdPercentage.Key, "0")
		defer paramtable.Get().Reset(paramtable.Get().QueryNodeCfg.OverloadedMemoryThresholdPercentage.Key)

		_, _, err := suite.loader.requestResource(context.Background())
		suite.NoError(err)
	})

	suite.Run("l0_segment_deltalog", func() {
		paramtable.Get().Save(paramtable.Get().QueryNodeCfg.DeltaDataExpansionRate.Key, "50")
		defer paramtable.Get().Reset(paramtable.Get().QueryNodeCfg.DeltaDataExpansionRate.Key)

		resource, _, err := suite.loader.requestResource(context.Background(), &querypb.SegmentLoadInfo{
			SegmentID:    100,
			CollectionID: suite.collectionID,
			Level:        datapb.SegmentLevel_L0,
			Deltalogs: []*datapb.FieldBinlog{
				{
					Binlogs: []*datapb.Binlog{
						{LogSize: 10000},
						{LogSize: 12000},
					},
				},
			},
		})

		suite.NoError(err)
		suite.EqualValues(1100000, resource.MemorySize)
	})
}

func TestSegmentLoader(t *testing.T) {
	suite.Run(t, &SegmentLoaderSuite{})
	suite.Run(t, &SegmentLoaderDetailSuite{})
}

type SegmentLoaderV2Suite struct {
	suite.Suite
	loader *segmentLoaderV2

	// Dependencies
	manager      *Manager
	rootPath     string
	chunkManager storage.ChunkManager

	// Data
	collectionID int64
	partitionID  int64
	segmentID    int64
	schema       *schemapb.CollectionSchema
	segmentNum   int
}

func (suite *SegmentLoaderV2Suite) SetupSuite() {
	paramtable.Init()
	suite.rootPath = suite.T().Name()
	suite.collectionID = rand.Int63()
	suite.partitionID = rand.Int63()
	suite.segmentID = rand.Int63()
	suite.segmentNum = 5
}

func (suite *SegmentLoaderV2Suite) SetupTest() {
	paramtable.Get().CommonCfg.EnableStorageV2.SwapTempValue("true")
	// Dependencies
	suite.manager = NewManager()
	ctx := context.Background()
	// TODO:: cpp chunk manager not support local chunk manager
	// suite.chunkManager = storage.NewLocalChunkManager(storage.RootPath(
	//	fmt.Sprintf("/tmp/milvus-ut/%d", rand.Int63())))
	chunkManagerFactory := NewTestChunkManagerFactory(paramtable.Get(), suite.rootPath)
	suite.chunkManager, _ = chunkManagerFactory.NewPersistentStorageChunkManager(ctx)
	suite.loader = NewLoaderV2(suite.manager, suite.chunkManager)
	initcore.InitRemoteChunkManager(paramtable.Get())

	// Data
	suite.schema = GenTestCollectionSchema("test", schemapb.DataType_Int64)
	indexMeta := GenTestIndexMeta(suite.collectionID, suite.schema)
	loadMeta := &querypb.LoadMetaInfo{
		LoadType:     querypb.LoadType_LoadCollection,
		CollectionID: suite.collectionID,
		PartitionIDs: []int64{suite.partitionID},
	}
	suite.manager.Collection.PutOrRef(suite.collectionID, suite.schema, indexMeta, loadMeta)
}

func (suite *SegmentLoaderV2Suite) TearDownTest() {
	ctx := context.Background()
	for i := 0; i < suite.segmentNum; i++ {
		suite.manager.Segment.Remove(suite.segmentID+int64(i), querypb.DataScope_All)
	}
	suite.chunkManager.RemoveWithPrefix(ctx, suite.rootPath)
	paramtable.Get().CommonCfg.EnableStorageV2.SwapTempValue("false")
}

func (suite *SegmentLoaderV2Suite) TestLoad() {
	tmpDir := suite.T().TempDir()
	paramtable.Get().CommonCfg.StorageScheme.SwapTempValue("file")
	paramtable.Get().CommonCfg.StoragePathPrefix.SwapTempValue(tmpDir)
	ctx := context.Background()

	msgLength := 4

	arrowSchema, err := typeutil.ConvertToArrowSchema(suite.schema.Fields)
	suite.NoError(err)
	opt := options.NewSpaceOptionBuilder().
		SetSchema(schema.NewSchema(
			arrowSchema,
			&schema.SchemaOptions{
				PrimaryColumn: "int64Field",
				VectorColumn:  "floatVectorField",
				VersionColumn: "Timestamp",
			})).
		Build()
	uri, err := typeutil.GetStorageURI("file", tmpDir, suite.segmentID)
	suite.NoError(err)
	space, err := milvus_storage.Open(uri, opt)
	suite.NoError(err)

	b := array.NewRecordBuilder(memory.DefaultAllocator, arrowSchema)
	defer b.Release()
	insertData, err := genInsertData(msgLength, suite.schema)
	suite.NoError(err)

	err = typeutil.BuildRecord(b, insertData, suite.schema.Fields)
	suite.NoError(err)
	rec := b.NewRecord()
	defer rec.Release()
	reader, err := array.NewRecordReader(arrowSchema, []arrow.Record{rec})
	suite.NoError(err)
	err = space.Write(reader, &options.DefaultWriteOptions)
	suite.NoError(err)

	collMeta := genCollectionMeta(suite.collectionID, suite.partitionID, suite.schema)
	inCodec := storage.NewInsertCodecWithSchema(collMeta)
	statsLog, err := inCodec.SerializePkStatsByData(insertData)
	suite.NoError(err)

	err = space.WriteBlob(statsLog.Value, statsLog.Key, false)
	suite.NoError(err)

	dschema := space.Manifest().GetSchema().DeleteSchema()
	dbuilder := array.NewRecordBuilder(memory.DefaultAllocator, dschema)
	defer dbuilder.Release()
	dbuilder.Field(0).(*array.Int64Builder).AppendValues([]int64{1, 2}, nil)
	dbuilder.Field(1).(*array.Int64Builder).AppendValues([]int64{100, 200}, nil)

	drec := dbuilder.NewRecord()
	defer drec.Release()

	dreader, err := array.NewRecordReader(dschema, []arrow.Record{drec})
	suite.NoError(err)

	err = space.Delete(dreader)
	suite.NoError(err)

	segments, err := suite.loader.Load(ctx, suite.collectionID, SegmentTypeSealed, 0, &querypb.SegmentLoadInfo{
		SegmentID:      suite.segmentID,
		PartitionID:    suite.partitionID,
		CollectionID:   suite.collectionID,
		NumOfRows:      int64(msgLength),
		StorageVersion: 3,
	})
	suite.NoError(err)

	_, err = suite.loader.LoadBloomFilterSet(ctx, suite.collectionID, 0, &querypb.SegmentLoadInfo{
		SegmentID:      suite.segmentID,
		PartitionID:    suite.partitionID,
		CollectionID:   suite.collectionID,
		NumOfRows:      int64(msgLength),
		StorageVersion: 3,
	})
	suite.NoError(err)

	segment := segments[0]
	suite.EqualValues(4, segment.InsertCount())
	suite.Equal(int64(msgLength-2), segment.RowNum())
}

func TestSegmentLoaderV2(t *testing.T) {
	suite.Run(t, &SegmentLoaderV2Suite{})
}
