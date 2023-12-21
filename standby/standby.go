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
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/signal"
	"go.uber.org/zap"
)

const (
	standbyState   = "standby"
	activatedState = "activated"

	exitWaitDuration = time.Duration(2) * time.Second
)

// ActivateRequest is the request body for activating the tidb server.
type ActivateRequest struct {
	KeyspaceName string `json:"keyspace_name"`
}

type sessionManager interface {
	ConnectionCount() int
	GetUserProcessList() map[uint64]*util.ProcessInfo
	GetClientCapabilityList() map[uint64]uint32
	KillAllConnections()
}

var (
	mu              sync.RWMutex
	state           = standbyState
	activateRequest ActivateRequest

	// activationTimeout specifies the maximum allowed time for tidb to activate from standby mode.
	activationTimeout uint
)

var activateCh = make(chan struct{}, 1)

// KeyspaceMismatch is the response body when the keyspace name in http request
// does not match the local keyspace name.
type KeyspaceMismatch struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}

func keyspaceChecker(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		remote := r.URL.Query().Get("keyspace")
		local := config.GetGlobalKeyspaceName()
		if remote != local {
			w.WriteHeader(http.StatusPreconditionFailed)
			mismatch := KeyspaceMismatch{
				Remote: remote,
				Local:  local,
			}
			body, err := json.Marshal(mismatch)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write(body)
			return
		}
		next.ServeHTTP(w, r)
	}
}

// Handler returns a handler to query tidb pool status or activate or exit the tidb server.
func Handler(sm sessionManager) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/tidb-pool/status", statusHandler)
	mux.HandleFunc("/tidb-pool/activate", func(w http.ResponseWriter, r *http.Request) {
		var req ActivateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.KeyspaceName == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		mu.Lock()
		if state == standbyState {
			state = activatedState
			activateRequest = req
			activateCh <- struct{}{}
		} else if activateRequest.KeyspaceName != req.KeyspaceName {
			mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			w.Write([]byte("server is not in standby mode"))
			return
		}
		// if client tries to activate with same keyspace name, wait for ready signal and return 200.
		mu.Unlock()

		timeout := make(<-chan time.Time)
		if activationTimeout > 0 {
			timeout = time.After(time.Duration(activationTimeout) * time.Second)
		}

		select {
		case <-r.Context().Done(): // client closed connection.
			go func() {
				EndStandby(errors.New("client closed connection"))
				signal.TiDBExit()
			}()
		case <-timeout: // reach hardlimit timeout from config.
			logutil.BgLogger().Warn("timeout waiting for activation")
			w.WriteHeader(http.StatusRequestTimeout)
			w.Write([]byte("timeout waiting for activation"))
			go func() {
				EndStandby(errors.New("timeout waiting for activation"))
				signal.TiDBExit()
			}()
		case <-serverStartCh:
			if startServerErr != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(startServerErr.Error()))
				return
			}
			statusHandler(w, r)
		}
	})
	mux.HandleFunc("/tidb-pool/exit", keyspaceChecker(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logutil.BgLogger().Info("receiving exit request, may exit after kill all connections...")
		if sm != nil {
			sm.KillAllConnections()
		}
		w.WriteHeader(http.StatusOK)
		signal.TiDBExit()
	})))
	return mux
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"state": "%s", "keyspace_name": "%s"}`, state, activateRequest.KeyspaceName)
}

var server *http.Server

// StartStandby starts a http server to listen and wait for activation signal.
func StartStandby(host string, port uint, timeout uint) ActivateRequest {
	mux := Handler(nil)
	// handle liveness probe.
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	// handle health
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"standby"}`)) })
	server = &http.Server{
		Handler: mux,
	}
	activationTimeout = timeout
	logutil.BgLogger().Info("tidb-server is now running as standby, waiting for activation...", zap.String("addr", server.Addr))
	go func() {
		addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
		l, err := net.Listen("tcp", addr)
		if err != nil {
			logutil.BgLogger().Warn("failed to listen", zap.Error(err))
			os.Exit(1)
		}
		clusterSecurity := config.GetGlobalConfig().Security.ClusterSecurity()
		tlsConfig, err := clusterSecurity.ToTLSConfig()
		if err != nil {
			logutil.BgLogger().Warn("failed to get tls config", zap.Error(err))
			os.Exit(1)
		}
		if tlsConfig != nil {
			l = tls.NewListener(l, tlsConfig)
		}
		if err := server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logutil.BgLogger().Warn("failed to start tidb-server as standby", zap.Error(err))
			os.Exit(1)
		}
	}()

	<-activateCh

	mu.RLock()
	defer mu.RUnlock()
	return activateRequest
}

var (
	serverStartCh  = make(chan struct{})
	startServerErr error
	endOnce        sync.Once
)

// EndStandby is used to notify the temp http server that the tidb server is ready or failed to init.
func EndStandby(err error) {
	endOnce.Do(func() {
		startServerErr = err
		close(serverStartCh)
		if server != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server.Shutdown(ctx)
		}
	})
}
