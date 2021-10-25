// Copyright (c) 2021 Nordix Foundation.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build linux

// Package ifconfig configures vpp instance with appropriate vlan interfaces for every NSC connection
package ifconfig

import (
	"context"
	"sync"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/vishvananda/netlink"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/next"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
)

const (
	add     = 0
	delete  = 1
	bufSize = 500
)

type ifOp struct {
	ctx    context.Context
	conn   *networkservice.Connection
	OpCode int
}

type ifConfigServer struct {
	stopWg       sync.WaitGroup
	ifOps        chan ifOp
	stop         chan interface{}
	parentIfName string
}

// Server network service server with stop method
type Server interface {
	networkservice.NetworkServiceServer
	Stop()
}

// NewServer creates new ifconfig server instance
func NewServer(parentIfName string) Server {
	ifServer := &ifConfigServer{parentIfName: parentIfName, ifOps: make(chan ifOp, bufSize),
		stop: make(chan interface{})}
	ifServer.stopWg.Add(1)
	go func() {
		defer ifServer.stopWg.Done()
		ifServer.handleIfOp()
	}()
	return ifServer
}

func (i *ifConfigServer) Request(ctx context.Context, request *networkservice.NetworkServiceRequest) (*networkservice.Connection, error) {
	//  TODO: perform is established check before writing ifOp into channel
	i.ifOps <- ifOp{ctx, request.GetConnection(), add}
	return next.Server(ctx).Request(ctx, request)
}

func (i *ifConfigServer) Close(ctx context.Context, conn *networkservice.Connection) (*empty.Empty, error) {
	// TODO: add conditional check
	i.ifOps <- ifOp{ctx, conn, delete}
	return next.Server(ctx).Close(ctx, conn)
}

func (i *ifConfigServer) Stop() {
	close(i.stop)
	i.stopWg.Wait()
}

func (i *ifConfigServer) handleIfOp() {
	for {
		select {
		case <-i.stop:
			return
		case ifOpObj := <-i.ifOps:
			switch ifOpObj.OpCode {
			case add:
				logger := log.FromContext(ifOpObj.ctx).WithField("handleIfOp", add)
				shouldReturn := i.handleIfOpAdd(logger, ifOpObj)
				if shouldReturn {
					return
				}
			case delete:
				// TODO: delete vlan sub interface from vpp instance
			}
		}
	}
}

func (i *ifConfigServer) handleIfOpAdd(logger log.Logger, ifOpObj ifOp) bool {
	for {
		select {
		case <-i.stop:
			return true
		default:
			done := make(chan struct{})
			linkUpdateCh := make(chan netlink.LinkUpdate)
			if err := netlink.LinkSubscribe(linkUpdateCh, done); err != nil {
				logger.Errorf("failed to subscribe interface update for %s", i.parentIfName)
				close(done)
				close(linkUpdateCh)
				return true
			}
			// find the link again to avoid the race
			parentIf, err := netlink.LinkByName(i.parentIfName)
			if err != nil {
				select {
				case <-i.stop:
					i.closeLinkSubscribe(done, linkUpdateCh)
					return true
				case linkUpdateEvent, ok := <-linkUpdateCh:
					i.closeLinkSubscribe(done, linkUpdateCh)
					if !ok {
						logger.Errorf("failed to receive interface update for %s", i.parentIfName)
						return true
					}
					if linkUpdateEvent.Link.Attrs().Name != i.parentIfName {
						logger.Infof("interface update received for: %s", linkUpdateEvent.Link.Attrs().Name)
						continue
					}
					parentIf, err = netlink.LinkByName(i.parentIfName)
					if err != nil {
						continue
					}
				}
			} else {
				i.closeLinkSubscribe(done, linkUpdateCh)
			}
			logger.Infof("parent interface %v: connection %v", parentIf, ifOpObj.conn.String())
			// TODO: add vlan sub interface on the vpp and configure ip, route etc.
		}
		break
	}
	return false
}

func (i *ifConfigServer) closeLinkSubscribe(done chan struct{}, linkUpdateCh chan netlink.LinkUpdate) {
	close(done)
	// `linkUpdateCh` should be fully read after the `done` close to prevent goroutine leak in `netlink.LinkSubscribe`
	go func() {
		for range linkUpdateCh {
		}
	}()
}
