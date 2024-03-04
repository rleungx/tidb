// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl

import (
	"context"
	"encoding/hex"
	"encoding/json"
	gerrors "errors"
	"fmt"
	"strconv"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/br/pkg/lightning/common"
	"github.com/pingcap/tidb/ddl/ingest"
	ddlutil "github.com/pingcap/tidb/ddl/util"
	"github.com/pingcap/tidb/disttask/framework/proto"
	"github.com/pingcap/tidb/disttask/framework/scheduler"
	"github.com/pingcap/tidb/disttask/framework/scheduler/execute"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

// MockDMLExecutionAddIndexSubTaskFinish is used to mock DML execution during distributed add index.
var MockDMLExecutionAddIndexSubTaskFinish func()

type backfillSchedulerHandle struct {
	*scheduler.BaseScheduler
	d         *ddl
	task      *proto.Task
	taskTable scheduler.TaskTable

	db            *model.DBInfo
	index         *model.IndexInfo
	job           *model.Job
	bc            ingest.BackendCtx
	ptbl          table.PhysicalTable
	jc            *JobContext
	eleTypeKey    []byte
	totalRowCnt   int64
	isPartition   bool
	stepForImport bool
	done          chan struct{}
	ctx           context.Context
}

// BackfillGlobalMeta is the global task meta for backfilling index.
type BackfillGlobalMeta struct {
	Job        model.Job `json:"job"`
	EleID      int64     `json:"ele_id"`
	EleTypeKey []byte    `json:"ele_type_key"`
}

// BackfillSubTaskMeta is the sub-task meta for backfilling index.
type BackfillSubTaskMeta struct {
	PhysicalTableID int64  `json:"physical_table_id"`
	StartKey        []byte `json:"start_key"`
	EndKey          []byte `json:"end_key"`
	SubTaskIndex    int    `json:"sub_task_index"`
}

func newBackfillDistScheduler(ctx context.Context, id string, task *proto.Task, taskTable scheduler.TaskTable, d *ddl) scheduler.Scheduler {
	s := &backfillSchedulerHandle{
		BaseScheduler: scheduler.NewBaseScheduler(ctx, id, task.ID, taskTable),
		d:             d,
		task:          task,
		taskTable:     taskTable,
		ctx:           ctx,
	}
	s.BaseScheduler.Extension = s
	return s
}

func (bh *backfillSchedulerHandle) Init(ctx context.Context) error {
	err := bh.BaseScheduler.Init(ctx)
	if err != nil {
		return err
	}
	d := bh.d

	bgm := &BackfillGlobalMeta{}
	err = json.Unmarshal(bh.task.Meta, bgm)
	if err != nil {
		return err
	}

	bh.eleTypeKey = bgm.EleTypeKey
	jobMeta := &bgm.Job
	bh.job = jobMeta

	db, tbl, err := d.getTableByTxn((*asAutoIDRequirement)(d.ddlCtx), jobMeta.SchemaID, jobMeta.TableID)
	if err != nil {
		return err
	}
	bh.isPartition = tbl.Meta().GetPartitionInfo() != nil
	bh.db = db

	physicalTable := tbl.(table.PhysicalTable)
	bh.ptbl = physicalTable

	indexInfo := model.FindIndexInfoByID(tbl.Meta().Indices, bgm.EleID)
	if indexInfo == nil {
		logutil.BgLogger().Warn("[ddl-ingest] cannot init cop request sender",
			zap.Int64("table ID", tbl.Meta().ID), zap.Int64("index ID", bgm.EleID))
		return errors.New("cannot find index info")
	}
	bh.index = indexInfo

	d.setDDLLabelForTopSQL(jobMeta.ID, jobMeta.Query)
	d.setDDLSourceForDiagnosis(jobMeta.ID, jobMeta.Type)
	jobCtx := d.jobContext(jobMeta.ID)
	bh.jc = jobCtx
	d.newReorgCtx(jobMeta.ID, 0)

	bc, err := ingest.LitBackCtxMgr.Register(d.ctx, bh.index.Unique, bh.job.ID, d.etcdCli)
	if err != nil {
		logutil.BgLogger().Warn("[ddl] lightning register error", zap.Error(err))
		return err
	}
	bh.bc = bc

	ser, err := infosync.GetServerInfo()
	if err != nil {
		return err
	}
	path := fmt.Sprintf("distAddIndex/%d/%s:%d", bh.job.ID, ser.IP, ser.Port)
	response, err := d.etcdCli.Get(ctx, path)
	if err != nil {
		return err
	}
	if len(response.Kvs) > 0 {
		cnt, err := strconv.Atoi(string(response.Kvs[0].Value))
		if err != nil {
			return err
		}
		bh.totalRowCnt = int64(cnt)
	}

	bh.done = make(chan struct{})
	go bh.UpdateStatLoop()

	return nil
}

func (s *backfillSchedulerHandle) IsIdempotent(_ *proto.Subtask) bool {
	return true
}

func (s *backfillSchedulerHandle) GetSubtaskExecutor(ctx context.Context, task *proto.Task, summary *execute.Summary) (execute.SubtaskExecutor, error) {
	s.stepForImport = task.Step == proto.StepTwo
	return &BackFillSubtaskExecutor{b: s}, nil
}

// UpdateStatLoop updates the row count of adding index.
func (b *backfillSchedulerHandle) UpdateStatLoop() {
	tk := time.Tick(time.Second * 5)
	ser, err := infosync.GetServerInfo()
	if err != nil {
		logutil.BgLogger().Warn("[ddl] get server info failed", zap.Error(err))
		return
	}
	path := fmt.Sprintf("%s/%d/%s:%d", rowCountEtcdPath, b.job.ID, ser.IP, ser.Port)
	writeToEtcd := func() {
		err := ddlutil.PutKVToEtcd(context.TODO(), b.d.etcdCli, 3, path, strconv.Itoa(int(b.totalRowCnt)))
		if err != nil {
			logutil.BgLogger().Warn("[ddl] update row count for distributed add index failed", zap.Error(err))
		}
	}
	for {
		select {
		case <-b.done:
			writeToEtcd()
			return
		case <-tk:
			writeToEtcd()
		}
	}
}

func (b *backfillSchedulerHandle) doFlushAndHandleError(mode ingest.FlushMode) error {
	_, _, err := b.bc.Flush(b.index.ID, mode)
	if err != nil {
		if common.ErrFoundDuplicateKeys.Equal(err) {
			err = convertToKeyExistsErr(err, b.index, b.ptbl.Meta())
		}
		logutil.BgLogger().Error("[ddl] flush error", zap.Error(err))
		return err
	}
	return nil
}

// Close implements the Scheduler interface.
func (b *backfillSchedulerHandle) Close() {
	if !gerrors.Is(b.ctx.Err(), context.Canceled) {
		// The context is upper layer's context, if the context is canceled, means the tidb is closed.
		// So we don't need to cleanup exec env.
		logutil.BgLogger().Info("[ddl] lightning cleanup subtask exec env")
		ingest.LitBackCtxMgr.Unregister(b.job.ID)
	}
	close(b.done)
	b.d.removeReorgCtx(b.job.ID)
	b.BaseScheduler.Close()
}

// Rollback implements the Scheduler interface.
func (b *backfillSchedulerHandle) Rollback(ctx context.Context, task *proto.Task) error {
	logutil.BgLogger().Info("[ddl] rollback backfill add index task", zap.Int64("jobID", b.job.ID))
	ingest.LitBackCtxMgr.Unregister(b.job.ID)
	b.d.removeReorgCtx(b.job.ID)
	return b.BaseScheduler.Rollback(ctx, task)
}

// BackFillSubtaskExecutor is the executor for backfill subtask.
type BackFillSubtaskExecutor struct {
	b *backfillSchedulerHandle
}

// Init is used to initialize the environment for the subtask executor.
func (e *BackFillSubtaskExecutor) Init(context.Context) error {
	return nil
}

// RunSubtask implements the Executor interface.
func (b *BackFillSubtaskExecutor) RunSubtask(ctx context.Context, task *proto.Subtask) error {
	logutil.BgLogger().Info("[ddl] run subtask", zap.Any("step", task.Step), zap.Int64("subtask", task.ID), zap.Int64("jobID", b.b.job.ID))

	fnCtx, fnCancel := context.WithCancel(context.Background())
	defer fnCancel()

	go func() {
		select {
		case <-ctx.Done():
			// If the task is cancelled, we should cancel the reorg worker.
			b.b.job.State = model.JobStateCancelled
			b.b.d.notifyReorgWorkerJobStateChange(b.b.job)
		case <-fnCtx.Done():
		}
	}()

	d := b.b.d
	sm := &BackfillSubTaskMeta{}
	err := json.Unmarshal(task.Meta, sm)
	if err != nil {
		logutil.BgLogger().Error("[ddl] unmarshal error", zap.Error(err))
		return err
	}

	if b.b.stepForImport {
		err = b.b.bc.Import(b.b.index.ID, b.b.index.Unique, b.b.ptbl)
		return nil
	}

	var startKey, endKey kv.Key
	var tbl table.PhysicalTable

	currentVer, err1 := getValidCurrentVersion(d.store)
	if err1 != nil {
		return errors.Trace(err1)
	}

	if !b.b.isPartition {
		startKey, endKey = sm.StartKey, sm.EndKey
		tbl = b.b.ptbl
	} else {
		pid := sm.PhysicalTableID
		parTbl := b.b.ptbl.(table.PartitionedTable)
		startKey, endKey, err = getTableRange(b.b.jc, d.ddlCtx, parTbl.GetPartition(pid), currentVer.Ver, b.b.job.Priority)
		if err != nil {
			logutil.BgLogger().Error("[ddl] get table range error", zap.Error(err))
			return err
		}
		tbl = parTbl.GetPartition(pid)
	}

	mockReorgInfo := &reorgInfo{Job: b.b.job, d: d.ddlCtx, StartKey: startKey, EndKey: endKey}
	elements := make([]*meta.Element, 0)
	elements = append(elements, &meta.Element{ID: b.b.index.ID, TypeKey: meta.IndexElementKey})
	mockReorgInfo.elements = elements
	mockReorgInfo.currElement = mockReorgInfo.elements[0]

	ingestScheduler := newIngestBackfillScheduler(b.b.ctx, mockReorgInfo, d.sessPool, tbl, true)
	defer ingestScheduler.close(true)

	consumer := newResultConsumer(d.ddlCtx, mockReorgInfo, nil, true)
	consumer.run(ingestScheduler, startKey, &b.b.totalRowCnt)

	err = ingestScheduler.setupWorkers()
	if err != nil {
		logutil.BgLogger().Error("[ddl] setup workers error", zap.Error(err))
		return err
	}

	baseID := genBaseBackfillTaskIDBySubTaskIndex(sm.SubTaskIndex)
	taskIDAlloc := newTaskIDAllocator(baseID)

	kvRanges := []kv.KeyRange{{StartKey: startKey, EndKey: endKey}}
	logutil.BgLogger().Info("[ddl] start backfill workers to reorg record",
		zap.Int("workerCnt", ingestScheduler.currentWorkerSize()),
		zap.Int("regionCnt", len(kvRanges)),
		zap.String("startKey", hex.EncodeToString(startKey)),
		zap.String("endKey", hex.EncodeToString(endKey)))

	sendTasks(ingestScheduler, consumer, tbl, kvRanges, mockReorgInfo, taskIDAlloc)

	ingestScheduler.close(false)
	if err := consumer.getResult(); err != nil {
		return err
	}

	return b.b.doFlushAndHandleError(ingest.FlushModeForceLocal)
}

// Cleanup implements the Executor interface.
func (b *BackFillSubtaskExecutor) Cleanup(context.Context) error {
	return nil
}

// OnFinished implements the Executor interface.
func (b *BackFillSubtaskExecutor) OnFinished(context.Context, *proto.Subtask) error {
	return nil
}

// Rollback implements the Executor interface.
func (b *BackFillSubtaskExecutor) Rollback(context.Context) error {
	return nil
}

const maxBackfillTaskCount = 100000000

func genBaseBackfillTaskIDBySubTaskIndex(subTaskIndex int) int {
	return subTaskIndex * maxBackfillTaskCount
}

// BackfillTaskType is the type of backfill task.
const BackfillTaskType = "backfill"
