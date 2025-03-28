// Copyright (c) 2021-2023 Nordix Foundation.
//
// Copyright (c) 2023-2024 Cisco Foundation.
//
// Copyright (c) 2024 OpenInfra Foundation Europe. All rights reserved.
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
// +build linux

// #nosec

package main

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/grpcfd"
	"github.com/kelseyhightower/envconfig"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"go.fd.io/govpp/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/networkservicemesh/vpphelper"
	"github.com/networkservicemesh/vpphelper/extendtimeout"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	kernelmech "github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/kernel"
	registryapi "github.com/networkservicemesh/api/pkg/api/registry"
	sriovtoken "github.com/networkservicemesh/sdk-sriov/pkg/networkservice/common/token"
	"github.com/networkservicemesh/sdk-sriov/pkg/tools/tokens"
	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/endpoint"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/authorize"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/kernel"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/kernel/vlan"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/recvfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/mechanisms/sendfd"
	"github.com/networkservicemesh/sdk/pkg/networkservice/common/onidle"
	"github.com/networkservicemesh/sdk/pkg/networkservice/connectioncontext/dnscontext"
	"github.com/networkservicemesh/sdk/pkg/networkservice/core/chain"
	"github.com/networkservicemesh/sdk/pkg/networkservice/ipam/point2pointipam"
	registryclient "github.com/networkservicemesh/sdk/pkg/registry/chains/client"
	registryauthorize "github.com/networkservicemesh/sdk/pkg/registry/common/authorize"
	registrysendfd "github.com/networkservicemesh/sdk/pkg/registry/common/sendfd"
	"github.com/networkservicemesh/sdk/pkg/tools/debug"
	"github.com/networkservicemesh/sdk/pkg/tools/dnsconfig"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
	"github.com/networkservicemesh/sdk/pkg/tools/opentelemetry"
	"github.com/networkservicemesh/sdk/pkg/tools/pprofutils"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"
	"github.com/networkservicemesh/sdk/pkg/tools/token"
	"github.com/networkservicemesh/sdk/pkg/tools/tracing"

	"github.com/networkservicemesh/cmd-nse-vlan-vpp/pkg/networkservice/ifconfig"
)

const (
	parentIfPrefix = "nse"
)

// Config holds configuration parameters from environment variables
type Config struct {
	Name                   string            `default:"vlan-vpp-responder" desc:"Name of vlan vpp responder"`
	BaseDir                string            `default:"./" desc:"base directory" split_words:"true"`
	ConnectTo              url.URL           `default:"unix:///var/lib/networkservicemesh/nsm.io.sock" desc:"url to connect to" split_words:"true"`
	MaxTokenLifetime       time.Duration     `default:"10m" desc:"maximum lifetime of tokens" split_words:"true"`
	RegistryClientPolicies []string          `default:"etc/nsm/opa/common/.*.rego,etc/nsm/opa/registry/.*.rego,etc/nsm/opa/client/.*.rego" desc:"paths to files and directories that contain registry client policies" split_words:"true"`
	ServiceNames           []string          `default:"vlan-vpp-responder" desc:"Name of provided services" split_words:"true"`
	Payload                string            `default:"ETHERNET" desc:"Name of provided service payload" split_words:"true"`
	Labels                 map[string]string `default:"" desc:"Endpoint labels"`
	DNSConfigs             dnsconfig.Decoder `default:"[]" desc:"DNSConfigs represents array of DNSConfig in json format. See at model definition: https://github.com/networkservicemesh/api/blob/main/pkg/api/networkservice/connectioncontext.pb.go#L426-L435" split_words:"true"`
	CidrPrefix             string            `default:"169.254.0.0/16" desc:"CIDR Prefix to assign IPs from" split_words:"true"`
	IdleTimeout            time.Duration     `default:"0" desc:"timeout for automatic shutdown when there were no requests for specified time. Set 0 to disable auto-shutdown." split_words:"true"`
	RegisterService        bool              `default:"true" desc:"if true then registers network service on startup" split_words:"true"`
	OpenTelemetryEndpoint  string            `default:"otel-collector.observability.svc.cluster.local:4317" desc:"OpenTelemetry Collector Endpoint" split_words:"true"`
	MetricsExportInterval  time.Duration     `default:"10s" desc:"interval between mertics exports" split_words:"true"`
	PprofEnabled           bool              `default:"false" desc:"is pprof enabled" split_words:"true"`
	PprofListenOn          string            `default:"localhost:6060" desc:"pprof URL to ListenAndServe" split_words:"true"`
	VPPMinOperationTimeout time.Duration     `default:"2s" desc:"minimum timeout for every vpp operation" split_words:"true"`
}

// Process prints and processes env to config
func (c *Config) Process() error {
	if err := envconfig.Usage("nsm", c); err != nil {
		return errors.Wrap(err, "cannot show usage of envconfig nse")
	}
	if err := envconfig.Process("nsm", c); err != nil {
		return errors.Wrap(err, "cannot process envconfig nse")
	}
	return nil
}

func logPhases(ctx context.Context) {
	// enumerating phases
	log.FromContext(ctx).Infof("there are 7 phases which will be executed followed by a success message:")
	log.FromContext(ctx).Infof("the phases include:")
	log.FromContext(ctx).Infof("1: get config from environment")
	log.FromContext(ctx).Infof("2: run vpp and get a connection to it")
	log.FromContext(ctx).Infof("3: retrieve spiffe svid")
	log.FromContext(ctx).Infof("4: create vlan-vpp-responder ipam")
	log.FromContext(ctx).Infof("5: create vlan-vpp-responder nse")
	log.FromContext(ctx).Infof("6: create grpc and mount nse")
	log.FromContext(ctx).Infof("7: register nse with nsm")
	log.FromContext(ctx).Infof("a final success message with start time duration")
}

func createNSEndpoint(ctx context.Context, source x509svid.Source, config *Config, vppConn api.Connection, ipnet *net.IPNet, cancel context.CancelFunc) (endpoint.Endpoint, ifconfig.Server) {
	sriovTokenVlanServer := getSriovTokenVlanServerChainElement(getTokenKey(ctx, tokens.FromEnv(os.Environ())))
	parentIfName := getParentIfname(config.Name)
	ifConfigServer := ifconfig.NewServer(ctx, parentIfName, vppConn)
	responderEndpoint := endpoint.NewServer(ctx,
		spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime),
		endpoint.WithName(config.Name),
		endpoint.WithAuthorizeServer(authorize.NewServer()),
		endpoint.WithAdditionalFunctionality(
			onidle.NewServer(ctx, cancel, config.IdleTimeout),
			point2pointipam.NewServer(ipnet),
			recvfd.NewServer(),
			mechanisms.NewServer(map[string]networkservice.NetworkServiceServer{
				kernelmech.MECHANISM: kernel.NewServer(kernel.WithInterfaceName(parentIfName)),
			}),
			sriovTokenVlanServer,
			ifConfigServer,
			dnscontext.NewServer(config.DNSConfigs...),
			sendfd.NewServer(),
		),
	)
	return responderEndpoint, ifConfigServer
}

func getParentIfname(nseName string) string {
	h := md5.New()
	_, err := io.WriteString(h, nseName)
	if err != nil {
		logrus.Fatalf("error generating parent interface name %+v", err)
	}
	nif := fmt.Sprintf("%s%x", parentIfPrefix, h.Sum(nil))
	return nif[:kernelmech.LinuxIfMaxLength]
}

func registerGRPCServer(tlsServerConfig *tls.Config, responderEndpoint *endpoint.Endpoint) *grpc.Server {
	options := append(
		tracing.WithTracing(),
		grpc.Creds(
			grpcfd.TransportCredentials(
				credentials.NewTLS(
					tlsServerConfig,
				),
			),
		),
	)
	server := grpc.NewServer(options...)
	(*responderEndpoint).Register(server)

	return server
}

func registerEndpoint(ctx context.Context, config *Config, source *workloadapi.X509Source, tlsClientConfig *tls.Config, urlStr string) error {
	clientOptions := append(
		tracing.WithTracingDial(),
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
			grpc.PerRPCCredentials(token.NewPerRPCCredentials(spiffejwt.TokenGeneratorFunc(source, config.MaxTokenLifetime)))),
		grpc.WithTransportCredentials(
			grpcfd.TransportCredentials(
				credentials.NewTLS(tlsClientConfig))),
		grpcfd.WithChainStreamInterceptor(),
		grpcfd.WithChainUnaryInterceptor(),
	)

	if config.RegisterService {
		for _, serviceName := range config.ServiceNames {
			nsRegistryClient := registryclient.NewNetworkServiceRegistryClient(ctx,
				registryclient.WithClientURL(&config.ConnectTo),
				registryclient.WithDialOptions(clientOptions...),
				registryclient.WithAuthorizeNSRegistryClient(registryauthorize.NewNetworkServiceRegistryClient()))
			_, err := nsRegistryClient.Register(ctx, &registryapi.NetworkService{
				Name:    serviceName,
				Payload: config.Payload,
			})

			if err != nil {
				log.FromContext(ctx).Fatalf("unable to register ns %+v", err)
			}
		}
	}

	nseRegistryClient := registryclient.NewNetworkServiceEndpointRegistryClient(
		ctx,
		registryclient.WithClientURL(&config.ConnectTo),
		registryclient.WithDialOptions(clientOptions...),
		registryclient.WithNSEAdditionalFunctionality(
			registrysendfd.NewNetworkServiceEndpointRegistryClient(),
		),
		registryclient.WithAuthorizeNSERegistryClient(registryauthorize.NewNetworkServiceEndpointRegistryClient(
			registryauthorize.WithPolicies(config.RegistryClientPolicies...),
		)),
	)
	nse := &registryapi.NetworkServiceEndpoint{
		Name:                 config.Name,
		NetworkServiceNames:  config.ServiceNames,
		NetworkServiceLabels: make(map[string]*registryapi.NetworkServiceLabels),
		Url:                  urlStr,
	}
	for _, serviceName := range config.ServiceNames {
		nse.NetworkServiceLabels[serviceName] = &registryapi.NetworkServiceLabels{Labels: config.Labels}
	}
	nse, err := nseRegistryClient.Register(ctx, nse)
	if err != nil {
		return err
	}
	logrus.Infof("nse: %+v", nse)

	return err
}

func setLogger(ctx context.Context) {
	log.EnableTracing(true)
	logrus.SetFormatter(&nested.Formatter{})
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[0]}))

	if err := debug.Self(); err != nil {
		log.FromContext(ctx).Infof("%s", err)
	}
}

func main() {
	// ********************************************************************************
	// setup context to catch signals
	// ********************************************************************************
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		// More Linux signals here
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	defer cancel()

	// ********************************************************************************
	// setup logging
	// ********************************************************************************
	setLogger(ctx)

	logPhases(ctx)

	starttime := time.Now()

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 1: get config from environment")
	// ********************************************************************************
	config := new(Config)
	if err := config.Process(); err != nil {
		logrus.Fatal(err.Error())
	}

	log.FromContext(ctx).Infof("Config: %#v", config)

	// ********************************************************************************
	// Configure Open Telemetry
	// ********************************************************************************
	if opentelemetry.IsEnabled() {
		collectorAddress := config.OpenTelemetryEndpoint
		spanExporter := opentelemetry.InitSpanExporter(ctx, collectorAddress)
		metricExporter := opentelemetry.InitOPTLMetricExporter(ctx, collectorAddress, config.MetricsExportInterval)
		o := opentelemetry.Init(ctx, spanExporter, metricExporter, config.Name)
		defer func() {
			if err := o.Close(); err != nil {
				log.FromContext(ctx).Error(err.Error())
			}
		}()
	}

	// ********************************************************************************
	// Configure pprof
	// ********************************************************************************
	if config.PprofEnabled {
		go pprofutils.ListenAndServe(ctx, config.PprofListenOn)
	}

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 2: run vpp and get a connection to it (time since start: %s)", time.Since(starttime))
	// ********************************************************************************
	vppConn, vppErrCh := vpphelper.StartAndDialContext(ctx)
	exitOnErr(ctx, cancel, vppErrCh)
	vppConn = extendtimeout.NewConnection(vppConn, config.VPPMinOperationTimeout)

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 3: retrieving svid, check spire agent logs if this is the last line you see")
	// ********************************************************************************
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	_, err = source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}

	tlsClientConfig := tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny())
	tlsClientConfig.MinVersion = tls.VersionTLS12
	tlsServerConfig := tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeAny())
	tlsServerConfig.MinVersion = tls.VersionTLS12

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 4: creating vlan-vpp-responder ipam")
	// ********************************************************************************
	_, ipnet, err := net.ParseCIDR(config.CidrPrefix)
	if err != nil {
		log.FromContext(ctx).Fatalf("error parsing cidr: %+v", err)
	}

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 5: create vlan-vpp-responder network service endpoint")
	// ********************************************************************************
	responderEndpoint, ifConfigServer := createNSEndpoint(ctx, source, config, vppConn, ipnet, cancel)
	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 6: create grpc server and register vlan-vpp-responder")
	// ********************************************************************************

	server := registerGRPCServer(tlsServerConfig, &responderEndpoint)
	tmpDir, err := os.MkdirTemp("", config.Name)
	if err != nil {
		logrus.Fatalf("error creating tmpDir %+v", err)
	}
	defer func(tmpDir string) { _ = os.Remove(tmpDir) }(tmpDir)
	listenOn := &(url.URL{Scheme: "unix", Path: filepath.Join(tmpDir, "listen.on")})
	srvErrCh := grpcutils.ListenAndServe(ctx, listenOn, server)
	exitOnErr(ctx, cancel, srvErrCh)
	log.FromContext(ctx).Infof("grpc server started")

	// ********************************************************************************
	log.FromContext(ctx).Infof("executing phase 7: register nse with nsm")
	// ********************************************************************************
	err = registerEndpoint(ctx, config, source, tlsClientConfig, listenOn.String())
	if err != nil {
		log.FromContext(ctx).Fatalf("failed to connect to registry: %+v", err)
	}

	// ********************************************************************************
	log.FromContext(ctx).Infof("startup completed in %v", time.Since(starttime))
	// ********************************************************************************

	// wait for server to exit
	<-ctx.Done()

	ifConfigServer.Stop()
}

func getSriovTokenVlanServerChainElement(tokenKey string) (tokenServer networkservice.NetworkServiceServer) {
	if tokenKey == "" {
		tokenServer = vlan.NewServer()
	} else {
		tokenServer = chain.NewNetworkServiceServer(
			vlan.NewServer(),
			sriovtoken.NewServer(tokenKey))
	}
	return
}

func getTokenKey(ctx context.Context, sriovTokens map[string][]string) string {
	if len(sriovTokens) > 1 {
		log.FromContext(ctx).Fatalf("endpoint must be configured with only one sriov resource")
	} else if len(sriovTokens) == 0 {
		return ""
	}
	var tokenKey string
	for tokenKey = range sriovTokens {
		break
	}
	if len(sriovTokens[tokenKey]) > 1 {
		log.FromContext(ctx).Fatalf("endpoint must be configured with only one VF on a sriov resource")
	}
	return tokenKey
}

func exitOnErr(ctx context.Context, cancel context.CancelFunc, errCh <-chan error) {
	// If we already have an error, log it and exit
	select {
	case err := <-errCh:
		log.FromContext(ctx).Fatal(err)
	default:
	}
	// Otherwise wait for an error in the background to log and cancel
	go func(ctx context.Context, errCh <-chan error) {
		err := <-errCh
		log.FromContext(ctx).Error(err)
		cancel()
	}(ctx, errCh)
}
