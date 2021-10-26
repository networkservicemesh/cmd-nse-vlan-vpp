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

	"github.com/edwarnicke/govpp/binapi/af_packet"
	interfaces "github.com/edwarnicke/govpp/binapi/interface"
	"github.com/edwarnicke/govpp/binapi/interface_types"
	"github.com/edwarnicke/vpphelper"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/kernel"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/next"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
)

const (
	add     = 0
	remove  = 1
	bufSize = 500
)

type ifOp struct {
	ctx    context.Context
	conn   *networkservice.Connection
	OpCode int
}

type ifConfigServer struct {
	stopWg          sync.WaitGroup
	ifOps           chan ifOp
	stop            chan interface{}
	parentIfName    string
	swIfIndex       interface_types.InterfaceIndex
	vppConn         vpphelper.Connection
	clientsRefCount int
	connections     map[string]interface{}
	mutex           sync.Mutex
}

// Server network service server with stop method
type Server interface {
	networkservice.NetworkServiceServer
	Stop()
}

// NewServer creates new ifconfig server instance
func NewServer(parentIfName string, vppConn vpphelper.Connection) Server {
	ifServer := &ifConfigServer{parentIfName: parentIfName, ifOps: make(chan ifOp, bufSize),
		stop: make(chan interface{}), vppConn: vppConn}
	ifServer.stopWg.Add(1)
	go func() {
		defer ifServer.stopWg.Done()
		ifServer.handleIfOp()
	}()
	return ifServer
}

func (i *ifConfigServer) Request(ctx context.Context, request *networkservice.NetworkServiceRequest) (*networkservice.Connection, error) {
	mechanism := kernel.ToMechanism(request.GetConnection().GetMechanism())
	if mechanism != nil && mechanism.GetVLAN() > 0 {
		i.mutex.Lock()
		connectionID := request.GetConnection().GetId()
		if _, exists := i.connections[connectionID]; exists {
			i.mutex.Unlock()
			return next.Server(ctx).Request(ctx, request)
		}
		i.connections[connectionID] = nil
		i.mutex.Unlock()

		i.ifOps <- ifOp{ctx, request.GetConnection(), add}
	}
	return next.Server(ctx).Request(ctx, request)
}

func (i *ifConfigServer) Close(ctx context.Context, conn *networkservice.Connection) (*empty.Empty, error) {
	mechanism := kernel.ToMechanism(conn.GetMechanism())
	if mechanism != nil && mechanism.GetVLAN() > 0 {
		i.mutex.Lock()
		connectionID := conn.GetId()
		if _, exists := i.connections[connectionID]; !exists {
			i.mutex.Unlock()
			return next.Server(ctx).Close(ctx, conn)
		}
		delete(i.connections, connectionID)
		i.mutex.Unlock()
		i.ifOps <- ifOp{ctx, conn, remove}
	}
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
				shouldReturn := i.handleIfOpAdd(ifOpObj.ctx, logger, ifOpObj)
				if shouldReturn {
					return
				}
			case remove:
				logger := log.FromContext(ifOpObj.ctx).WithField("handleIfOp", remove)
				err := i.addDeleteVppParentIf(ifOpObj.ctx, ifOpObj.conn, false)
				if err != nil {
					logger.Errorf("error handling parent interface on vpp: %v", err)
				}
				// TODO: delete vlan sub interface from vpp instance
			}
		}
	}
}

func (i *ifConfigServer) handleIfOpAdd(ctx context.Context, logger log.Logger, ifOpObj ifOp) bool {
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
					err = i.addDeleteVppParentIf(ctx, ifOpObj.conn, true)
					if err != nil {
						logger.Errorf("error processing parent interface on vpp: %v", err)
						return false
					}
				}
			} else {
				i.closeLinkSubscribe(done, linkUpdateCh)
				err = i.addDeleteVppParentIf(ctx, ifOpObj.conn, true)
				if err != nil {
					logger.Errorf("error handling parent interface on vpp: %v", err)
					return false
				}
			}
			logger.Infof("parent interface %v: connection %v", parentIf, ifOpObj.conn.String())
			// TODO: add vlan sub interface on the vpp and configure ip, route etc.
		}
		break
	}
	return false
}

func (i *ifConfigServer) addDeleteVppParentIf(ctx context.Context, conn *networkservice.Connection, isAdd bool) error {
	i.mutex.Lock()
	defer i.mutex.Unlock()
	if isAdd {
		i.clientsRefCount++
		if i.clientsRefCount > 1 {
			return nil
		}
		// TODO: revisit this to support RDMA config on the parent interface
		if kernel.ToMechanism(conn.GetMechanism()).GetDeviceTokenID() != "" {
			return errors.Errorf("only raw socket config supported on parent kernel interface %v", conn)
		}
		// create parent interface on vpp when first ns client shows up
		rsp, err := af_packet.NewServiceClient(i.vppConn).AfPacketCreate(ctx, &af_packet.AfPacketCreate{
			HostIfName:      i.parentIfName,
			UseRandomHwAddr: true,
		})
		if err != nil {
			return err
		}
		if _, err := interfaces.NewServiceClient(i.vppConn).SwInterfaceSetRxMode(ctx, &interfaces.SwInterfaceSetRxMode{
			SwIfIndex: rsp.SwIfIndex,
			Mode:      interface_types.RX_MODE_API_ADAPTIVE,
		}); err != nil {
			return err
		}
		if _, err := interfaces.NewServiceClient(i.vppConn).SwInterfaceSetFlags(ctx, &interfaces.SwInterfaceSetFlags{
			SwIfIndex: rsp.SwIfIndex,
			Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
		}); err != nil {
			return err
		}
		i.swIfIndex = rsp.SwIfIndex
	} else {
		i.clientsRefCount--
		if i.clientsRefCount > 0 {
			return nil
		}
		// delete parent interface from vpp when last ns client is deleted
		_, err := af_packet.NewServiceClient(i.vppConn).AfPacketDelete(ctx, &af_packet.AfPacketDelete{
			HostIfName: i.parentIfName,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *ifConfigServer) closeLinkSubscribe(done chan struct{}, linkUpdateCh chan netlink.LinkUpdate) {
	close(done)
	// `linkUpdateCh` should be fully read after the `done` close to prevent goroutine leak in `netlink.LinkSubscribe`
	go func() {
		for range linkUpdateCh {
		}
	}()
}
