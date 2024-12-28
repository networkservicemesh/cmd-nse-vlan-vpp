// Copyright (c) 2021-2022 Nordix Foundation.
//
// Copyright (c) 2023-2024 Cisco Foundation.
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

//go:build linux

// Package ifconfig configures vpp instance with appropriate vlan interfaces for every NSC connection
package ifconfig

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"go.fd.io/govpp/api"

	"github.com/networkservicemesh/govpp/binapi/af_packet"
	"github.com/networkservicemesh/govpp/binapi/fib_types"
	interfaces "github.com/networkservicemesh/govpp/binapi/interface"
	"github.com/networkservicemesh/govpp/binapi/interface_types"
	"github.com/networkservicemesh/govpp/binapi/ip"
	"github.com/networkservicemesh/govpp/binapi/rdma"

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
	conn   *networkservice.Connection
	OpCode int
}

type ifConfigServer struct {
	ifCtx           context.Context
	stopWg          sync.WaitGroup
	ifOps           chan ifOp
	stop            chan interface{}
	parentIfName    string
	swIfIndexesMap  map[string]interface_types.InterfaceIndex
	vppConn         api.Connection
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
func NewServer(ctx context.Context, parentIfName string, vppConn api.Connection) Server {
	ifServer := &ifConfigServer{ifCtx: ctx, parentIfName: parentIfName, ifOps: make(chan ifOp, bufSize),
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
		connectionKey := i.getConnectionKey(mechanism.GetVLAN())
		if _, exists := i.connections[connectionKey]; exists {
			i.mutex.Unlock()
			return next.Server(ctx).Request(ctx, request)
		}
		i.connections[connectionKey] = nil
		i.mutex.Unlock()

		i.ifOps <- ifOp{request.GetConnection(), add}
	}
	return next.Server(ctx).Request(ctx, request)
}

func (i *ifConfigServer) Close(ctx context.Context, conn *networkservice.Connection) (*empty.Empty, error) {
	mechanism := kernel.ToMechanism(conn.GetMechanism())
	if mechanism != nil && mechanism.GetVLAN() > 0 {
		i.mutex.Lock()
		connectionKey := i.getConnectionKey(mechanism.GetVLAN())
		if _, exists := i.connections[connectionKey]; !exists {
			i.mutex.Unlock()
			return next.Server(ctx).Close(ctx, conn)
		}
		delete(i.connections, connectionKey)
		i.mutex.Unlock()
		i.ifOps <- ifOp{conn, remove}
	}
	return next.Server(ctx).Close(ctx, conn)
}

func (i *ifConfigServer) Stop() {
	close(i.stop)
	i.stopWg.Wait()
}

func (i *ifConfigServer) getConnectionKey(vlanID uint32) string {
	return i.parentIfName + "." + fmt.Sprint(vlanID)
}

func (i *ifConfigServer) handleIfOp() {
	for {
		select {
		case <-i.stop:
			return
		case ifOpObj := <-i.ifOps:
			switch ifOpObj.OpCode {
			case add:
				logger := log.FromContext(i.ifCtx).WithField("handleIfOp", "add")
				shouldReturn := i.handleIfOpAdd(i.ifCtx, logger, ifOpObj)
				if shouldReturn {
					return
				}
			case remove:
				logger := log.FromContext(i.ifCtx).WithField("handleIfOp", "remove")
				err := i.removeVlanSubInterface(i.ifCtx, logger, ifOpObj.conn)
				if err != nil {
					logger.Errorf("error deleting vlan sub-interface on vpp: %v", err)
				}
				err = i.addDeleteVppParentIf(i.ifCtx, logger, ifOpObj.conn, false)
				if err != nil {
					logger.Errorf("error handling removal of parent interface on vpp: %v", err)
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
			_, err := netlink.LinkByName(i.parentIfName)
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
				_, err = netlink.LinkByName(i.parentIfName)
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
							logger.Infof("interface update event: actual: %s expected: %s", linkUpdateEvent.Link.Attrs().Name, i.parentIfName)
							continue
						}
						_, err = netlink.LinkByName(i.parentIfName)
						if err != nil {
							continue
						}
					}
				} else {
					i.closeLinkSubscribe(done, linkUpdateCh)
				}
			}
			err = i.addDeleteVppParentIf(ctx, logger, ifOpObj.conn, true)
			if err != nil {
				logger.Errorf("error handling parent interface on vpp: %v", err)
				return false
			}
			logger.Infof("add vlan sub interface: parent interface %s: connection %v", i.parentIfName, ifOpObj.conn.String())
			err = i.addVlanSubInterface(ctx, logger, ifOpObj.conn)
			if err != nil {
				logger.Errorf("error adding vlan sub interface for connection: %v, err: %v", ifOpObj.conn, err)
			}
		}
		break
	}
	return false
}

func (i *ifConfigServer) removeVlanSubInterface(ctx context.Context, logger log.Logger, conn *networkservice.Connection) error {
	var swVLANIfIndex interface_types.InterfaceIndex
	var ok bool
	connectionKey := i.getConnectionKey(kernel.ToMechanism(conn.GetMechanism()).GetVLAN())
	if swVLANIfIndex, ok = i.swIfIndexesMap[connectionKey]; !ok {
		return errors.Errorf("vlan interface not found for connection %v", conn)
	}

	err := i.ipaddressAddDel(ctx, swVLANIfIndex, conn.GetContext().GetIpContext().GetDstIPNets(), false)
	if err != nil {
		return err
	}

	err = i.routeAddDel(ctx, swVLANIfIndex, conn.GetContext().GetIpContext().GetSrcIPRoutes(), false)
	if err != nil {
		return err
	}

	_, err = interfaces.NewServiceClient(i.vppConn).DeleteSubif(ctx, &interfaces.DeleteSubif{
		SwIfIndex: swVLANIfIndex,
	})
	if err != nil {
		return err
	}

	delete(i.swIfIndexesMap, connectionKey)
	logger.Infof("vlan sub interface removed for connection: %v", conn)
	return nil
}

func (i *ifConfigServer) addVlanSubInterface(ctx context.Context, logger log.Logger, conn *networkservice.Connection) error {
	var swParentIfIndex interface_types.InterfaceIndex
	var ok bool
	if swParentIfIndex, ok = i.swIfIndexesMap[i.parentIfName]; !ok {
		return errors.Errorf("parent interface not found for connection %v", conn)
	}
	vlanID := kernel.ToMechanism(conn.GetMechanism()).GetVLAN()
	rsp, err := interfaces.NewServiceClient(i.vppConn).CreateVlanSubif(ctx, &interfaces.CreateVlanSubif{
		SwIfIndex: swParentIfIndex,
		VlanID:    vlanID,
	})
	if err != nil {
		return err
	}

	if _, err = interfaces.NewServiceClient(i.vppConn).SwInterfaceSetFlags(ctx, &interfaces.SwInterfaceSetFlags{
		SwIfIndex: rsp.SwIfIndex,
		Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
	}); err != nil {
		return err
	}

	err = i.ipaddressAddDel(ctx, rsp.SwIfIndex, conn.GetContext().GetIpContext().GetDstIPNets(), true)
	if err != nil {
		return err
	}

	err = i.routeAddDel(ctx, rsp.SwIfIndex, conn.GetContext().GetIpContext().GetSrcIPRoutes(), true)
	if err != nil {
		return err
	}

	i.swIfIndexesMap[i.getConnectionKey(vlanID)] = rsp.SwIfIndex
	logger.Infof("vlan sub interface configured for VLAN ID %d, if index %v, connection %v", vlanID, i.swIfIndexesMap[conn.GetId()], conn)
	return nil
}

func (i *ifConfigServer) ipaddressAddDel(ctx context.Context, swIfIndex interface_types.InterfaceIndex, ipNets []*net.IPNet, isAdd bool) error {
	if ipNets == nil {
		return nil
	}
	for _, ipNet := range ipNets {
		if _, err := interfaces.NewServiceClient(i.vppConn).SwInterfaceAddDelAddress(ctx, &interfaces.SwInterfaceAddDelAddress{
			SwIfIndex: swIfIndex,
			IsAdd:     isAdd,
			Prefix:    types.ToVppAddressWithPrefix(ipNet),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (i *ifConfigServer) routeAddDel(ctx context.Context, swIfIndex interface_types.InterfaceIndex, routes []*networkservice.Route, isAdd bool) error {
	if routes == nil {
		return nil
	}
	for _, route := range routes {
		if route.GetPrefixIPNet() == nil {
			return errors.New("vppRoute prefix must not be nil")
		}
		vppRoute := i.toRoute(route, swIfIndex)

		if _, err := ip.NewServiceClient(i.vppConn).IPRouteAddDel(ctx, &ip.IPRouteAddDel{
			IsAdd:       isAdd,
			IsMultipath: false,
			Route:       vppRoute,
		}); err != nil {
			return err
		}
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

func (i *ifConfigServer) addDeleteVppParentIf(ctx context.Context, logger log.Logger, conn *networkservice.Connection, isAdd bool) error {
	if isAdd {
		if i.clientsRefCount > 0 {
			i.clientsRefCount++
			return nil
		}
		// create parent interface on vpp when first ns client shows up
		var swIfIndex interface_types.InterfaceIndex
		if kernel.ToMechanism(conn.GetMechanism()).GetDeviceTokenID() != "" {
			// create RDMA interface as parent interface as it is a VF
			rsp, err := rdma.NewServiceClient(i.vppConn).RdmaCreateV3(ctx, &rdma.RdmaCreateV3{
				HostIf: i.parentIfName,
				Name:   i.parentIfName,
			})
			if err != nil {
				return err
			}
			swIfIndex = rsp.SwIfIndex
		} else {
			// create af packet interface as parent interface as it is a veth interface
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
			swIfIndex = rsp.SwIfIndex
		}
		err := i.makeIfOpUp(ctx, swIfIndex)
		if err != nil {
			return err
		}
		i.swIfIndexesMap[i.parentIfName] = swIfIndex
		i.clientsRefCount++
		logger.Infof("parent interface %s is added into vpp, if index %v", i.parentIfName, i.swIfIndexesMap[i.parentIfName])
	} else {
		if i.clientsRefCount > 1 {
			i.clientsRefCount--
			return nil
		}
		// delete parent interface from vpp when last ns client is deleted
		if kernel.ToMechanism(conn.GetMechanism()).GetDeviceTokenID() != "" {
			if swIfIndex, ok := i.swIfIndexesMap[i.parentIfName]; ok {
				_, err := rdma.NewServiceClient(i.vppConn).RdmaDelete(ctx, &rdma.RdmaDelete{
					SwIfIndex: swIfIndex,
				})
				if err != nil {
					return err
				}
			}
		} else {
			_, err := af_packet.NewServiceClient(i.vppConn).AfPacketDelete(ctx, &af_packet.AfPacketDelete{
				HostIfName: i.parentIfName,
			})
			if err != nil {
				return err
			}
		}
		delete(i.swIfIndexesMap, i.parentIfName)
		i.clientsRefCount--
		logger.Infof("parent interface %s is deleted from vpp", i.parentIfName)
	}
	return nil
}

func (i *ifConfigServer) makeIfOpUp(ctx context.Context, swIfIndex interface_types.InterfaceIndex) error {
	if _, err := interfaces.NewServiceClient(i.vppConn).SwInterfaceSetFlags(ctx, &interfaces.SwInterfaceSetFlags{
		SwIfIndex: swIfIndex,
		Flags:     interface_types.IF_STATUS_API_FLAG_ADMIN_UP,
	}); err != nil {
		return err
	}
	return nil
}

// nolint: revive
func (i *ifConfigServer) closeLinkSubscribe(done chan struct{}, linkUpdateCh chan netlink.LinkUpdate) {
	close(done)
	// `linkUpdateCh` should be fully read after the `done` close to prevent goroutine leak in `netlink.LinkSubscribe`
	for range linkUpdateCh {
	}
}
