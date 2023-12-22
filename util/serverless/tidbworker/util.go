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

package tidbworker

import (
	"strconv"

	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

const (
	workerIDPrefix = "tidb-worker-"
)

// IsMaster returns whether the current TiDB is of role master.
func IsMaster() bool {
	return GlobalTiDBWorkerManager != nil && GlobalTiDBWorkerManager.Role() == config.RoleMaster
}

// IsBgTaskMaster returns whether the current TiDB is of role bg job master.
func IsBgTaskMaster(taskType string) bool {
	// Check if the current TiDB has worker enabled.
	if GlobalTiDBWorkerManager == nil {
		return false
	}
	switch TaskWorkerType(taskType) {
	case WorkerTypeDDL:
		return config.GetGlobalConfig().TiDBWorker.DDLWorkerCount > 0
	case WorkerTypeBatch:
		return config.GetGlobalConfig().TiDBWorker.BatchWorkerCount > 0
	default:
		logutil.BgLogger().Warn("[tidb-worker] unsupported task type for tidb worker", zap.String("task-type", taskType))
	}
	return false
}

// IsGCWorker returns whether the current TiDB is a GC worker.
func IsGCWorker() bool {
	return GlobalTiDBWorkerManager != nil && GlobalTiDBWorkerManager.Role() == config.RoleGCWorker
}

// IsGCV2Worker returns whether the current TiDB is a GCV2 worker.
func IsGCV2Worker() bool {
	return GlobalTiDBWorkerManager != nil && GlobalTiDBWorkerManager.Role() == config.RoleGCV2Worker
}

// SchedulerNodes generate scheduler nodes according to tidb worker config instead of current
// cluster topology.
func SchedulerNodes(workerType string, gTaskID int64) []*infosync.ServerInfo {
	var nodeCount int
	switch workerType {
	case WorkerTypeDDL:
		nodeCount = config.GetGlobalConfig().TiDBWorker.DDLWorkerCount
	case WorkerTypeBatch:
		nodeCount = config.GetGlobalConfig().TiDBWorker.BatchWorkerCount
	default:
		return nil
	}
	nodes := make([]*infosync.ServerInfo, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodes[i] = &infosync.ServerInfo{
			IP:   workerIDPrefix + workerType + "-" + strconv.FormatInt(gTaskID, 10),
			Port: uint(i),
		}
	}
	return nodes
}

// IsWorkerExecID checks whether the execID belongs to a tidb worker.
func IsWorkerExecID(execID, workerType string) bool {
	prefix := workerIDPrefix + workerType + "-"
	return len(execID) > len(prefix) && execID[:len(prefix)] == prefix
}
