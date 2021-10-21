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
	"strings"
	"sync"
	"time"

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
		case ifOp := <-i.ifOps:
			switch ifOp.OpCode {
			case add:
				logger := log.FromContext(ifOp.ctx).WithField("handleIfOp", add)
				for {
					select {
					case <-i.stop:
						return
					default:
						parentIf, err := netlink.LinkByName(i.parentIfName)
						if err != nil {
							if strings.Contains(err.Error(), "Link not found") {
								time.Sleep(1 * time.Second)
								continue
							}
							logger.Errorf("failed to get link for %q - %v", i.parentIfName, err)
							return
						}
						logger.Infof("parent interface %v: connection %v", parentIf, ifOp.conn.String())
						// TODO: add vlan sub interface on the vpp and configure ip, route etc.
					}
					break
				}
			case delete:
				// TODO: delete vlan sub interface from vpp instance
			}
		}
	}
}
