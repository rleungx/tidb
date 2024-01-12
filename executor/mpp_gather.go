// Copyright 2020 PingCAP, Inc.
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

package executor

import (
	"context"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/distsql"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/model"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/memory"
)

func useMPPExecution(ctx sessionctx.Context, tr *plannercore.PhysicalTableReader) bool {
	if !ctx.GetSessionVars().IsMPPAllowed() {
		return false
	}
	_, ok := tr.GetTablePlan().(*plannercore.PhysicalExchangeSender)
	return ok
}

func getMPPQueryID(ctx sessionctx.Context) uint64 {
	mppQueryInfo := &ctx.GetSessionVars().StmtCtx.MPPQueryInfo
	mppQueryInfo.QueryID.CompareAndSwap(0, plannercore.AllocMPPQueryID())
	return mppQueryInfo.QueryID.Load()
}

func getMPPQueryTS(ctx sessionctx.Context) uint64 {
	mppQueryInfo := &ctx.GetSessionVars().StmtCtx.MPPQueryInfo
	mppQueryInfo.QueryTS.CompareAndSwap(0, uint64(time.Now().UnixNano()))
	return mppQueryInfo.QueryTS.Load()
}

// MPPGather dispatch MPP tasks and read data from root tasks.
type MPPGather struct {
	// following fields are construct needed
	baseExecutor
	is           infoschema.InfoSchema
	originalPlan plannercore.PhysicalPlan
	startTS      uint64
	mppQueryID   kv.MPPQueryID

	respIter distsql.SelectResult

	memTracker *memory.Tracker

	// For virtual column.
	columns                    []*model.ColumnInfo
	virtualColumnIndex         []int
	virtualColumnRetFieldTypes []*types.FieldType

	// For UnionScan.
	table    table.Table
	kvRanges []kv.KeyRange
	dummy    bool

	mppExec *MPPExecWithRetry
}

func collectPlanIDs(plan plannercore.PhysicalPlan, ids []int) []int {
	ids = append(ids, plan.ID())
	for _, child := range plan.Children() {
		ids = collectPlanIDs(child, ids)
	}
	return ids
}

// Open implement Executor interface.
func (e *MPPGather) Open(ctx context.Context) (err error) {
	if e.dummy {
		sender, ok := e.originalPlan.(*plannercore.PhysicalExchangeSender)
		if !ok {
			return errors.Errorf("unexpected plan type, expect: PhysicalExchangeSender, got: %s", e.originalPlan.TP())
		}
		if _, e.kvRanges, _, err = plannercore.GenerateRootMPPTasks(e.ctx, 0, e.startTS, e.mppQueryID, sender, e.is); err != nil {
			return err
		}
		return nil
	}

	planIDs := collectPlanIDs(e.originalPlan, nil)
	if e.mppExec, err = NewMPPExecWithRetry(ctx, e.ctx, e.memTracker, planIDs, e.originalPlan, e.startTS, e.mppQueryID, e.is); err != nil {
		return err
	}
	e.kvRanges = e.mppExec.KVRanges
	e.respIter = distsql.GenSelectResultFromMPPResponse(e.ctx, e.retFieldTypes, planIDs, e.id, e.mppExec)
	return nil
}

// Next fills data into the chunk passed by its caller.
func (e *MPPGather) Next(ctx context.Context, chk *chunk.Chunk) error {
	chk.Reset()
	if e.dummy {
		return nil
	}
	if err := e.respIter.Next(ctx, chk); err != nil {
		return err
	}
	if chk.NumRows() == 0 {
		return nil
	}
	return table.FillVirtualColumnValue(e.virtualColumnRetFieldTypes, e.virtualColumnIndex, e.Schema().Columns, e.columns, e.ctx, chk)
}

// Close and release the used resources.
func (e *MPPGather) Close() error {
	if e.dummy {
		return nil
	}
	if e.respIter != nil {
		return e.respIter.Close()
	}
	return nil
}

// Table implements the dataSourceExecutor interface.
func (e *MPPGather) Table() table.Table {
	return e.table
}

func (e *MPPGather) setDummy() {
	e.dummy = true
}
