// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package standby

import (
	"sync/atomic"
	"time"

	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/signal"
	"go.uber.org/zap"
)

var lastActive int64

// UpdateLastActive makes sure `lastActive` is not less than current time.
func UpdateLastActive(t time.Time) {
	for {
		last := atomic.LoadInt64(&lastActive)
		if last >= t.Unix() {
			return
		}
		if atomic.CompareAndSwapInt64(&lastActive, last, t.Unix()) {
			return
		}
	}
}

// StartWatchLastActive watches `lastActive` and exits the process if it is not updated for a long time.
func StartWatchLastActive(sm sessionManager, maxIdleSecs int) {
	UpdateLastActive(time.Now())
	go func() {
		ticker := time.NewTicker(time.Second * 10)
		defer ticker.Stop()
		for {
			<-ticker.C
			last := atomic.LoadInt64(&lastActive)
			if time.Now().Unix()-last > int64(maxIdleSecs) {
				connCount := sm.ConnectionCount()
				var processCount, inTransCount int
				for _, p := range sm.GetUserProcessList() {
					if p.Command != mysql.ComSleep { // ignore sleep sessions (waiting for client query).
						processCount++
					}
					if p.State&mysql.ServerStatusInTrans > 0 {
						inTransCount++
					}
				}
				var clientInteractiveCount int
				for _, c := range sm.GetClientCapabilityList() {
					if c&mysql.ClientInteractive > 0 {
						clientInteractiveCount++
					}
				}
				logutil.BgLogger().Info("connection idle for too long",
					zap.Int("max-idle-seconds", maxIdleSecs),
					zap.Int("connection-count", connCount),
					zap.Int("process-count", processCount),
					zap.Int("inTrans-count", inTransCount),
					zap.Int("client-interactive-count", clientInteractiveCount))

				if (connCount == 0 || processCount == 0) && inTransCount == 0 && clientInteractiveCount == 0 {
					// insure no active connections due to skip grace wait, exit.
					sm.KillAllConnections()
					signal.TiDBExit()
				}
			}
		}
	}()
}
