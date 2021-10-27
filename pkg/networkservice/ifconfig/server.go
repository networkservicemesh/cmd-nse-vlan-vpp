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
	"github.com/edwarnicke/govpp/binapi/fib_types"
	interfaces "github.com/edwarnicke/govpp/binapi/interface"
	"github.com/edwarnicke/govpp/binapi/interface_types"
	"github.com/edwarnicke/govpp/binapi/ip"
	"github.com/edwarnicke/vpphelper"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/kernel"
	"github.com/networkservicemesh/sdk-vpp/pkg/tools/types"
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
	swIfIndexesMap  map[string]interface_types.InterfaceIndex
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
		swIfIndexesMap: make(map[string]interface_types.InterfaceIndex), stop: make(chan interface{}),
		vppConn: vppConn, connections: make(map[string]interface{})}
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
					logger.Errorf("error handling removal of parent interface on vpp: %v", err)
				}
				err = i.removeVlanSubInterface(ifOpObj.ctx, ifOpObj.conn)
				if err != nil {
					logger.Errorf("error deleting vlan sub-interface on vpp: %v", err)
				}
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
			parentIf, err := netlink.LinkByName(i.parentIfName)
			if err != nil {
				done := make(chan struct{})
				linkUpdateCh := make(chan netlink.LinkUpdate)
				if err = netlink.LinkSubscribe(linkUpdateCh, done); err != nil {
					logger.Errorf("failed to subscribe interface update for %s", i.parentIfName)
					close(done)
					close(linkUpdateCh)
					return true
				}
				// find the link again to avoid the race
				parentIf, err = netlink.LinkByName(i.parentIfName)
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
			}
			logger.Infof("add vlan sub interface: parent interface %v: connection %v", parentIf, ifOpObj.conn.String())
			err = i.addVlanSubInterface(ctx, ifOpObj.conn)
			if err != nil {
				logger.Errorf("error adding vlan sub interface for connection %v: %v", ifOpObj.conn, err)
			}
		}
		break
	}
	return false
}

func (i *ifConfigServer) removeVlanSubInterface(ctx context.Context, conn *networkservice.Connection) error {
	var swVLANIfIndex interface_types.InterfaceIndex
	var ok bool
	if swVLANIfIndex, ok = i.swIfIndexesMap[conn.GetId()]; !ok {
		return errors.Errorf("vlan interface not found for connection %v", conn)
	}
	_, err := interfaces.NewServiceClient(i.vppConn).DeleteSubif(ctx, &interfaces.DeleteSubif{
		SwIfIndex: swVLANIfIndex,
	})
	if err != nil {
		return err
	}
	delete(i.swIfIndexesMap, conn.GetId())
	return nil
}

func (i *ifConfigServer) addVlanSubInterface(ctx context.Context, conn *networkservice.Connection) error {
	var swParentIfIndex interface_types.InterfaceIndex
	var ok bool
	if swParentIfIndex, ok = i.swIfIndexesMap[i.parentIfName]; !ok {
		return errors.Errorf("parent interface not found for connection %v", conn)
	}
	rsp, err := interfaces.NewServiceClient(i.vppConn).CreateVlanSubif(ctx, &interfaces.CreateVlanSubif{
		SwIfIndex: swParentIfIndex,
		VlanID:    kernel.ToMechanism(conn.GetMechanism()).GetVLAN(),
	})
	if err != nil {
		return err
	}
	err = i.makeIfOpUp(ctx, rsp.SwIfIndex)
	if err != nil {
		return err
	}
	ipNets := conn.GetContext().GetIpContext().GetSrcIPNets()
	if ipNets == nil {
		return nil
	}
	for _, ipNet := range ipNets {
		if _, err := interfaces.NewServiceClient(i.vppConn).SwInterfaceAddDelAddress(ctx, &interfaces.SwInterfaceAddDelAddress{
			SwIfIndex: rsp.SwIfIndex,
			IsAdd:     true,
			Prefix:    types.ToVppAddressWithPrefix(ipNet),
		}); err != nil {
			return err
		}
	}
	routes := conn.GetContext().GetIpContext().GetSrcIPRoutes()
	if routes == nil {
		return nil
	}
	for _, route := range routes {
		if err := i.routeAdd(ctx, rsp.SwIfIndex, route); err != nil {
			return err
		}
	}
	i.swIfIndexesMap[conn.GetId()] = rsp.SwIfIndex
	return nil
}

func (i *ifConfigServer) routeAdd(ctx context.Context, swIfIndex interface_types.InterfaceIndex, route *networkservice.Route) error {
	if route.GetPrefixIPNet() == nil {
		return errors.New("vppRoute prefix must not be nil")
	}
	vppRoute := i.toRoute(route, swIfIndex)

	if _, err := ip.NewServiceClient(i.vppConn).IPRouteAddDel(ctx, &ip.IPRouteAddDel{
		IsAdd:       true,
		IsMultipath: false,
		Route:       vppRoute,
	}); err != nil {
		return err
	}

	return nil
}

func (i *ifConfigServer) toRoute(route *networkservice.Route, via interface_types.InterfaceIndex) ip.IPRoute {
	prefix := route.GetPrefixIPNet()
	rv := ip.IPRoute{
		StatsIndex: 0,
		Prefix:     types.ToVppPrefix(prefix),
		NPaths:     1,
		Paths: []fib_types.FibPath{
			{
				SwIfIndex: uint32(via),
				TableID:   0,
				RpfID:     0,
				Weight:    1,
				Type:      fib_types.FIB_API_PATH_TYPE_NORMAL,
				Flags:     fib_types.FIB_API_PATH_FLAG_NONE,
				Proto:     types.IsV6toFibProto(prefix.IP.To4() == nil),
			},
		},
	}
	nh := route.GetNextHopIP()
	if nh != nil {
		rv.Paths[0].Nh.Address = types.ToVppAddress(nh).Un
	}
	return rv
}

func (i *ifConfigServer) addDeleteVppParentIf(ctx context.Context, conn *networkservice.Connection, isAdd bool) error {
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
		err = i.makeIfOpUp(ctx, rsp.SwIfIndex)
		if err != nil {
			return err
		}
		i.swIfIndexesMap[i.parentIfName] = rsp.SwIfIndex
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
		delete(i.swIfIndexesMap, i.parentIfName)
	}
	return nil
}

func (i *ifConfigServer) makeIfOpUp(ctx context.Context, swIfIndex interface_types.InterfaceIndex) error {
	if _, err := interfaces.NewServiceClient(i.vppConn).SwInterfaceSetRxMode(ctx, &interfaces.SwInterfaceSetRxMode{
		SwIfIndex: swIfIndex,
		Mode:      interface_types.RX_MODE_API_ADAPTIVE,
	}); err != nil {
		return err
	}
	if _, err := interfaces.NewServiceClient(i.vppConn).SwInterfaceSetFlags(ctx, &interfaces.SwInterfaceSetFlags{
		SwIfIndex: swIfIndex,
		Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
	}); err != nil {
		return err
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
