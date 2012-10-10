// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zk

import (
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"launchpad.net/gozk/zookeeper"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

/* When you need to talk to multiple zk cells, you need a simple
abstraction so you aren't caching clients all over the place.

ConnCache guarantees that you have at most one zookeeper connection per cell.
*/

type cachedConn struct {
	mutex sync.Mutex // used to notify if multiple goroutine simultaneously want a connection
	zconn Conn
}

type ConnCache struct {
	mutex          sync.Mutex
	zconnCellMap   map[string]*cachedConn // map cell name to connection
	connectTimeout time.Duration
	useZkocc       bool
}

func (cc *ConnCache) ConnForPath(zkPath string) (cn Conn, err error) {
	zcell := ZkCellFromZkPath(zkPath)

	cc.mutex.Lock()
	if cc.zconnCellMap == nil {
		cc.mutex.Unlock()
		return nil, &zookeeper.Error{Op: "dial", Code: zookeeper.ZCLOSING}
	}

	conn, ok := cc.zconnCellMap[zcell]
	if !ok {
		conn = &cachedConn{}
		cc.zconnCellMap[zcell] = conn
	}
	cc.mutex.Unlock()

	// We only want one goroutine at a time trying to connect here, so keep the
	// lock during the zk dial process.
	conn.mutex.Lock()
	defer conn.mutex.Unlock()

	if conn.zconn != nil {
		return conn.zconn, nil
	}

	if cc.useZkocc {
		conn.zconn, err = cc.newZkoccConn(zkPath, zcell)
	} else {
		conn.zconn, err = cc.newZookeeperConn(zkPath, zcell)
	}
	return conn.zconn, err
}

func (cc *ConnCache) newZookeeperConn(zkPath, zcell string) (Conn, error) {
	zconn, session, err := zookeeper.Dial(ZkPathToZkAddr(zkPath, false), cc.connectTimeout)
	if err == nil {
		// Wait for connection.
		// FIXME(msolomon) the deadlines seems to be a bit fuzzy, need to double check
		// and potentially do a high-level select here.
		event := <-session
		if event.State != zookeeper.STATE_CONNECTED {
			err = fmt.Errorf("zk connect failed: %v", event.State)
		}
		if err == nil {
			go cc.handleSessionEvents(zcell, zconn, session)
			return NewZkConn(zconn), nil
		} else {
			zconn.Close()
		}
	}
	return nil, err
}

func (cc *ConnCache) handleSessionEvents(cell string, conn *zookeeper.Conn, session <-chan zookeeper.Event) {
	for event := range session {
		switch event.State {
		case zookeeper.STATE_EXPIRED_SESSION:
			conn.Close()
			fallthrough
		case zookeeper.STATE_CLOSED:
			cc.mutex.Lock()
			if cc.zconnCellMap != nil {
				delete(cc.zconnCellMap, cell)
			}
			cc.mutex.Unlock()
			log.Printf("zk conn cache: session for cell %v ended: %v", cell, event)
			return
		default:
			log.Printf("zk conn cache: session for cell %v event: %v", cell, event)
		}
	}
}

// from the zkPath (of the form server1:port1,server2:port2,server3:port3:...)
// splits it on commas, randomizes the list, and tries to connect
// to the servers, stopping at the first successful connection
func (cc *ConnCache) newZkoccConn(zkPath, zcell string) (Conn, error) {
	servers := strings.Split(ZkPathToZkAddr(zkPath, true), ",")
	perm := rand.Perm(len(servers))
	for _, index := range perm {
		server := servers[index]
		zconn, err := DialZkocc(server)
		if err == nil {
			return zconn, nil
		}
		log.Printf("zk conn cache: zkocc connection to %v failed: %v", server, err)
	}
	return nil, fmt.Errorf("zkocc connect failed: %v", zkPath)
}

func (cc *ConnCache) Close() error {
	cc.mutex.Lock()
	defer cc.mutex.Unlock()
	for _, conn := range cc.zconnCellMap {
		conn.mutex.Lock()
		if conn.zconn != nil {
			conn.zconn.Close()
			conn.zconn = nil
		}
		conn.mutex.Unlock()
	}
	cc.zconnCellMap = nil
	return nil
}

func NewConnCache(connectTimeout time.Duration, useZkocc bool) *ConnCache {
	return &ConnCache{
		zconnCellMap:   make(map[string]*cachedConn),
		connectTimeout: connectTimeout,
		useZkocc:       useZkocc}
}
