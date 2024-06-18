// Copyright 2024 EMQ Technologies Co., Ltd.
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

package connection

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/pingcap/failpoint"

	"github.com/lf-edge/ekuiper/contract/v2/api"
	"github.com/lf-edge/ekuiper/v2/internal/conf"
	"github.com/lf-edge/ekuiper/v2/internal/topo/context"
	"github.com/lf-edge/ekuiper/v2/pkg/errorx"
	"github.com/lf-edge/ekuiper/v2/pkg/modules"
)

func storeConnectionMeta(plugin, id string, props map[string]interface{}) error {
	err := conf.WriteCfgIntoKVStorage("connections", plugin, id, props)
	failpoint.Inject("storeConnectionErr", func() {
		err = errors.New("storeConnectionErr")
	})
	return err
}

func dropConnectionStore(plugin, id string) error {
	err := conf.DropCfgKeyFromStorage("connections", plugin, id)
	failpoint.Inject("dropConnectionStoreErr", func() {
		err = errors.New("dropConnectionStoreErr")
	})
	return err
}

func GetConnectionRef(id string) int {
	globalConnectionManager.RLock()
	defer globalConnectionManager.RUnlock()
	meta, ok := globalConnectionManager.connectionPool[id]
	if !ok {
		return 0
	}
	return meta.refCount
}

func GetAllConnectionStatus(ctx api.StreamContext) map[string]ConnectionStatus {
	globalConnectionManager.RLock()
	defer globalConnectionManager.RUnlock()
	s := make(map[string]ConnectionStatus)
	for id, err := range globalConnectionManager.failConnection {
		status := ConnectionStatus{
			Status: ConnectionFail,
			ErrMsg: err,
		}
		s[id] = status
	}
	for id, meta := range globalConnectionManager.connectionPool {
		status := ConnectionStatus{
			Status: ConnectionRunning,
		}
		err := meta.conn.Ping(ctx)
		if err != nil {
			status.Status = ConnectionFail
			status.ErrMsg = err.Error()
		}
		s[id] = status
	}
	return s
}

func GetAllConnectionsID() []string {
	globalConnectionManager.RLock()
	defer globalConnectionManager.RUnlock()
	ids := make([]string, 0)
	for key := range globalConnectionManager.connectionPool {
		ids = append(ids, key)
	}
	return ids
}

func PingConnection(ctx api.StreamContext, id string) error {
	if id == "" {
		return fmt.Errorf("connection id should be defined")
	}
	globalConnectionManager.RLock()
	defer globalConnectionManager.RUnlock()
	meta, ok := globalConnectionManager.connectionPool[id]
	if !ok {
		return fmt.Errorf("connection %s not existed", id)
	}
	return meta.conn.Ping(ctx)
}

func FetchConnection(ctx api.StreamContext, id, typ string, props map[string]interface{}) (modules.Connection, error) {
	if id == "" {
		return nil, fmt.Errorf("connection id should be defined")
	}
	selID := extractSelID(props)
	if len(selID) < 1 {
		return CreateNonStoredConnection(ctx, id, typ, props)
	}
	globalConnectionManager.Lock()
	defer globalConnectionManager.Unlock()
	return attachConnection(selID)
}

func attachConnection(id string) (modules.Connection, error) {
	if id == "" {
		return nil, fmt.Errorf("connection id should be defined")
	}
	meta, ok := globalConnectionManager.connectionPool[id]
	if !ok {
		return nil, fmt.Errorf("connection %s not existed", id)
	}
	meta.refCount++
	return meta.conn, nil
}

func DetachConnection(ctx api.StreamContext, id string, props map[string]interface{}) error {
	if id == "" {
		return fmt.Errorf("connection id should be defined")
	}
	globalConnectionManager.Lock()
	defer globalConnectionManager.Unlock()
	selID := extractSelID(props)
	if len(selID) < 1 {
		return detachConnection(ctx, id, true)
	}
	return detachConnection(ctx, selID, false)
}

func detachConnection(ctx api.StreamContext, id string, remove bool) error {
	meta, ok := globalConnectionManager.connectionPool[id]
	if !ok {
		return nil
	}
	if remove {
		conn := meta.conn
		conn.Close(ctx)
		delete(globalConnectionManager.connectionPool, id)
		return nil
	}
	meta.refCount--
	return nil
}

func CreateNamedConnection(ctx api.StreamContext, id, typ string, props map[string]any) (modules.Connection, error) {
	if id == "" || typ == "" {
		return nil, fmt.Errorf("connection id and type should be defined")
	}
	globalConnectionManager.Lock()
	defer globalConnectionManager.Unlock()
	_, ok := globalConnectionManager.connectionPool[id]
	if ok {
		return nil, fmt.Errorf("connection %v already been created", id)
	}
	meta := &ConnectionMeta{
		ID:    id,
		Typ:   typ,
		Props: props,
	}
	if err := storeConnectionMeta(typ, id, props); err != nil {
		return nil, err
	}
	conn, err := createNamedConnection(ctx, meta)
	if err != nil {
		return nil, err
	}
	meta.conn = conn
	globalConnectionManager.connectionPool[id] = meta
	if _, ok := globalConnectionManager.failConnection[id]; ok {
		delete(globalConnectionManager.failConnection, id)
	}
	return conn, nil
}

func CreateNonStoredConnection(ctx api.StreamContext, id, typ string, props map[string]any) (modules.Connection, error) {
	if id == "" || typ == "" {
		return nil, fmt.Errorf("connection id and type should be defined")
	}
	globalConnectionManager.Lock()
	defer globalConnectionManager.Unlock()
	_, ok := globalConnectionManager.connectionPool[id]
	if ok {
		return nil, fmt.Errorf("connection %v already been created", id)
	}
	meta := &ConnectionMeta{
		ID:    id,
		Typ:   typ,
		Props: props,
	}
	conn, err := createNamedConnection(ctx, meta)
	if err != nil {
		return nil, err
	}
	meta.conn = conn
	globalConnectionManager.connectionPool[id] = meta
	return conn, nil
}

var mockErr = true

func createNamedConnection(ctx api.StreamContext, meta *ConnectionMeta) (modules.Connection, error) {
	var conn modules.Connection
	var err error
	connRegister, ok := modules.ConnectionRegister[strings.ToLower(meta.Typ)]
	if !ok {
		return nil, fmt.Errorf("unknown connection type")
	}
	err = backoff.Retry(func() error {
		conn, err = connRegister(ctx, meta.Props)
		failpoint.Inject("createConnectionErr", func() {
			if mockErr {
				err = errorx.NewIOErr("createConnectionErr")
				mockErr = false
			}
		})
		if err == nil {
			return nil
		}
		if errorx.IsIOError(err) {
			return err
		}
		return backoff.Permanent(err)
	}, NewExponentialBackOff())
	return conn, err
}

func DropNameConnection(ctx api.StreamContext, selId string) error {
	if selId == "" {
		return fmt.Errorf("connection id should be defined")
	}
	globalConnectionManager.Lock()
	defer globalConnectionManager.Unlock()
	meta, ok := globalConnectionManager.connectionPool[selId]
	if !ok {
		_, ok := globalConnectionManager.failConnection[selId]
		if ok {
			delete(globalConnectionManager.failConnection, selId)
		}
		return nil
	}
	if meta.refCount > 0 {
		return fmt.Errorf("connection %s can't be dropped due to reference", selId)
	}
	err := dropConnectionStore(meta.Typ, selId)
	if err != nil {
		return fmt.Errorf("drop connection %s failed, err:%v", selId, err)
	}
	meta.conn.Close(ctx)
	delete(globalConnectionManager.connectionPool, selId)
	return nil
}

var globalConnectionManager *ConnectionManager

func InitConnectionManager4Test() error {
	InitMockTest()
	InitConnectionManager()
	return nil
}

func InitConnectionManager() {
	globalConnectionManager = &ConnectionManager{
		connectionPool: make(map[string]*ConnectionMeta),
		failConnection: make(map[string]string),
	}
	if conf.IsTesting {
		return
	}
	DefaultBackoffMaxElapsedDuration = time.Duration(conf.Config.Connection.BackoffMaxElapsedDuration)
}

func ReloadConnection() error {
	cfgs, err := conf.GetCfgFromKVStorage("connections", "", "")
	if err != nil {
		return err
	}
	for key, props := range cfgs {
		names := strings.Split(key, ".")
		if len(names) != 3 {
			continue
		}
		typ := names[1]
		id := names[2]
		meta := &ConnectionMeta{
			ID:    id,
			Typ:   typ,
			Props: props,
		}
		conn, err := createNamedConnection(context.Background(), meta)
		if err != nil {
			conf.Log.Warnf("initialize connection:%v failed, err:%v", id, err)
			globalConnectionManager.failConnection[id] = err.Error()
			continue
		}
		meta.conn = conn
		globalConnectionManager.connectionPool[id] = meta
	}
	return nil
}

type ConnectionManager struct {
	sync.RWMutex
	connectionPool map[string]*ConnectionMeta
	failConnection map[string]string
}

type ConnectionMeta struct {
	ID       string             `json:"id"`
	Typ      string             `json:"typ"`
	Props    map[string]any     `json:"props"`
	conn     modules.Connection `json:"-"`
	refCount int                `json:"-"`
}

func NewExponentialBackOff() *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(DefaultInitialInterval),
		backoff.WithMaxInterval(DefaultMaxInterval),
		backoff.WithMaxElapsedTime(DefaultBackoffMaxElapsedDuration),
	)
}

const (
	DefaultInitialInterval = 100 * time.Millisecond
	DefaultMaxInterval     = 1 * time.Second
)

var DefaultBackoffMaxElapsedDuration = 3 * time.Minute

const (
	ConnectionRunning = "running"
	ConnectionFail    = "fail"
)

type ConnectionStatus struct {
	Status string
	ErrMsg string
}

func extractSelID(props map[string]interface{}) string {
	if len(props) < 1 {
		return ""
	}
	v, ok := props["connectionSelector"]
	if !ok {
		return ""
	}
	id, ok := v.(string)
	if !ok {
		return ""
	}
	return id
}