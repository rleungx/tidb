// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/disttask/framework/dispatcher"
	"github.com/pingcap/tidb/disttask/framework/handle"
	"github.com/pingcap/tidb/disttask/framework/proto"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/store/helper"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/util/backoff"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/serverless/tidbworker"

	"github.com/tikv/client-go/v2/tikv"
	"go.uber.org/zap"
)

const (
	scanRegionBackoffBase = 200 * time.Millisecond
	scanRegionBackoffMax  = 2 * time.Second
)

type backfillDispatcher struct {
	*dispatcher.BaseDispatcher
	d *ddl
}

func newBackfillDispatcher(ctx context.Context, d *ddl, taskMgr dispatcher.TaskManager,
	serverID string, task *proto.Task) dispatcher.Dispatcher {
	dsp := backfillDispatcher{
		d:              d,
		BaseDispatcher: dispatcher.NewBaseDispatcher(ctx, taskMgr, serverID, task),
	}
	dsp.Extension = &backfillExtension{d}
	return &dsp
}

type backfillExtension struct {
	d *ddl
}

func (backfillExtension) OnTick(_ context.Context, _ *proto.Task) {
}

// StepStr convert proto.Step to string.
func StepStr(step proto.Step) string {
	switch step {
	case proto.StepInit:
		return "init"
	case proto.StepOne:
		return "run"
	case proto.StepDone:
		return "done"
	default:
		return "unknown"
	}
}

func (b backfillExtension) OnNextSubtasksBatch(ctx context.Context, h dispatcher.TaskHandle, gTask *proto.Task, serverInfo []*infosync.ServerInfo, step proto.Step) (subtaskMetas [][]byte, err error) {
	logger := logutil.BgLogger().With(
		zap.Stringer("type", gTask.Type),
		zap.Int64("task-id", gTask.ID),
		zap.String("curr-step", StepStr(gTask.Step)),
		zap.String("next-step", StepStr(step)),
	)

	var globalTaskMeta BackfillGlobalMeta
	if err = json.Unmarshal(gTask.Meta, &globalTaskMeta); err != nil {
		return nil, err
	}
	logger.Info("on next subtasks batch")
	if step == proto.StepDone {
		return nil, nil
	}

	job := &globalTaskMeta.Job
	var tblInfo *model.TableInfo
	err = kv.RunInNewTxn(b.d.ctx, b.d.store, true, func(ctx context.Context, txn kv.Transaction) error {
		tblInfo, err = meta.NewMeta(txn).GetTable(job.SchemaID, job.TableID)
		return err
	})

	var subTaskMetas [][]byte
	if tblInfo.Partition == nil {
		if gTask.Step == proto.StepOne {
			dummyMeta := &BackfillSubTaskMeta{}
			metaBytes, err := json.Marshal(dummyMeta)
			if err != nil {
				return nil, err
			}

			return [][]byte{metaBytes}, nil
		}
		tbl, err := getTable((*asAutoIDRequirement)(b.d.ddlCtx), job.SchemaID, tblInfo)
		if err != nil {
			return nil, err
		}
		ver, err := getValidCurrentVersion(b.d.store)
		if err != nil {
			return nil, errors.Trace(err)
		}
		startKey, endKey, err := getTableRange(b.d.jobContext(job.ID), b.d.ddlCtx, tbl.(table.PhysicalTable), ver.Ver, job.Priority)
		if startKey == nil && endKey == nil {
			// Empty table.
			return nil, nil
		}
		if err != nil {
			return nil, errors.Trace(err)
		}

		subTaskMetas = make([][]byte, 0, 100)
		backoffer := backoff.NewExponential(scanRegionBackoffBase, 2, scanRegionBackoffMax)
		err = handle.RunWithRetry(b.d.ctx, 8, backoffer, logutil.Logger(b.d.ctx), func(_ context.Context) (bool, error) {
			regionCache := b.d.store.(helper.Storage).GetRegionCache()
			recordRegionMetas, err := regionCache.LoadRegionsInKeyRange(tikv.NewBackofferWithVars(context.Background(), 20000, nil), startKey, endKey)
			if err != nil {
				return false, err
			}
			sort.Slice(recordRegionMetas, func(i, j int) bool {
				return bytes.Compare(recordRegionMetas[i].StartKey(), recordRegionMetas[j].StartKey()) < 0
			})

			// Check if regions are continuous.
			shouldRetry := false
			cur := recordRegionMetas[0]
			for _, m := range recordRegionMetas[1:] {
				if !bytes.Equal(cur.EndKey(), m.StartKey()) {
					shouldRetry = true
					break
				}
				cur = m
			}
			if shouldRetry {
				return true, nil
			}
			regionBatch := 4
			if config.GetGlobalConfig().BackfillRegionBatch != 0 {
				regionBatch = config.GetGlobalConfig().BackfillRegionBatch
			}
			for i := 0; i < len(recordRegionMetas); i += regionBatch {
				end := i + regionBatch
				if end > len(recordRegionMetas) {
					end = len(recordRegionMetas)
				}
				batch := recordRegionMetas[i:end]
				subTaskMeta := &BackfillSubTaskMeta{StartKey: batch[0].StartKey(), EndKey: batch[len(batch)-1].EndKey(), SubTaskIndex: i / regionBatch}
				if i == 0 {
					subTaskMeta.StartKey = startKey
				}
				if end == len(recordRegionMetas) {
					subTaskMeta.EndKey = endKey
				}
				metaBytes, err := json.Marshal(subTaskMeta)
				if err != nil {
					return false, err
				}
				subTaskMetas = append(subTaskMetas, metaBytes)
			}
			logutil.BgLogger().Info("generate subtask", zap.Int("subtask count", len(subTaskMetas)), zap.Int("region count", len(recordRegionMetas)))
			return false, nil
		})
		if len(subTaskMetas) == 0 {
			return nil, errors.Errorf("regions are not continuous")
		}
	} else {
		if gTask.Step == proto.StepOne {
			dummyMeta := &BackfillSubTaskMeta{}
			metaBytes, err := json.Marshal(dummyMeta)
			if err != nil {
				return nil, err
			}
			return [][]byte{metaBytes}, nil
		}
		defs := tblInfo.Partition.Definitions
		physicalIDs := make([]int64, len(defs))
		for i := range defs {
			physicalIDs[i] = defs[i].ID
		}

		subTaskMetas = make([][]byte, 0, len(physicalIDs))
		for i, physicalID := range physicalIDs {
			subTaskMeta := &BackfillSubTaskMeta{
				PhysicalTableID: physicalID,
				SubTaskIndex:    i,
			}

			metaBytes, err := json.Marshal(subTaskMeta)
			if err != nil {
				return nil, err
			}

			subTaskMetas = append(subTaskMetas, metaBytes)
		}
		logutil.BgLogger().Info("generate subtask", zap.Int("subtask count", len(subTaskMetas)), zap.Int("partition count", len(physicalIDs)))
	}

	return subTaskMetas, nil
}

// OnErrStage generate error handling stage's plan.
func (backfillExtension) OnErrStage(_ context.Context, _ dispatcher.TaskHandle, task *proto.Task, receiveErrs []error) (meta []byte, err error) {
	// We do not need extra meta info when rolling back
	logger := logutil.BgLogger().With(
		zap.Stringer("type", task.Type),
		zap.Int64("task-id", task.ID),
		zap.String("step", StepStr(task.Step)),
	)
	logger.Info("on error stage", zap.Errors("errors", receiveErrs))
	firstErr := receiveErrs[0]
	task.Error = firstErr

	return nil, nil
}

func (backfillExtension) OnDone(_ context.Context, _ dispatcher.TaskHandle, _ *proto.Task) error {
	return nil
}

func (backfillExtension) GetEligibleInstances(ctx context.Context, task *proto.Task) ([]*infosync.ServerInfo, bool, error) {
	// Return placeholder nodes according to setting if tidb worker for ddl is enabled.
	if variable.EnableDistTask.Load() && tidbworker.IsBgTaskMaster(string(task.Type)) {
		return tidbworker.SchedulerNodes(tidbworker.TaskWorkerType(string(task.Type)), task.ID), false, nil
	}
	serverInfos, err := dispatcher.GenerateSchedulerNodes(ctx)
	if err != nil {
		return nil, true, err
	}
	return serverInfos, true, nil
}

func (backfillExtension) IsRetryableErr(error) bool {
	return true
}

func (backfillExtension) GetNextStep(task *proto.Task) proto.Step {
	switch task.Step {
	case proto.StepInit:
		return proto.StepOne
	case proto.StepOne:
		return proto.StepTwo
	default:
		return proto.StepDone
	}
}
