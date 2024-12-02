# Build

## Build cmd binary locally

You can build the locally by executing

```bash
go build ./...
```

## Build Docker container

You can build the docker container by running:

```bash
docker build .
```

# Usage

## Environment config

* `NSM_NAME`                        - Name of vlan vpp responder (default: "vlan-vpp-responder")
* `NSM_BASE_DIR`                    - base directory (default: "./")
* `NSM_CONNECT_TO`                  - url to connect to (default: "unix:///var/lib/networkservicemesh/nsm.io.sock")
* `NSM_MAX_TOKEN_LIFETIME`          - maximum lifetime of tokens (default: "10m")
* `NSM_REGISTRY_CLIENT_POLICIES`    - paths to files and directories that contain registry client policies (default: "etc/nsm/opa/common/.*.rego,etc/nsm/opa/registry/.*.rego,etc/nsm/opa/client/.*.rego")
* `NSM_SERVICE_NAMES`               - Name of provided services (default: "vlan-vpp-responder")
* `NSM_PAYLOAD`                     - Name of provided service payload (default: "ETHERNET")
* `NSM_LABELS`                      - Endpoint labels
* `NSM_DNS_CONFIGS`                 - DNSConfigs represents array of DNSConfig in json format. See at model definition: https://github.com/networkservicemesh/api/blob/main/pkg/api/networkservice/connectioncontext.pb.go#L426-L435 (default: "[]")
* `NSM_CIDR_PREFIX`                 - CIDR Prefix to assign IPs from (default: "169.254.0.0/16")
* `NSM_IDLE_TIMEOUT`                - timeout for automatic shutdown when there were no requests for specified time. Set 0 to disable auto-shutdown. (default: "0")
* `NSM_REGISTER_SERVICE`            - if true then registers network service on startup (default: "true")
* `NSM_OPEN_TELEMETRY_ENDPOINT`     - OpenTelemetry Collector Endpoint (default: "otel-collector.observability.svc.cluster.local:4317")
* `NSM_METRICS_EXPORT_INTERVAL`     - interval between mertics exports (default: "10s")
* `NSM_PPROF_ENABLED`               - is pprof enabled (default: "false")
* `NSM_PPROF_LISTEN_ON`             - pprof URL to ListenAndServe (default: "localhost:6060")
* `NSM_VPP_MIN_OPERATION_TIMEOUT`   - minimum timeout for every vpp operation

# Testing

## Testing Docker container

Testing is run via a Docker container.  To run testing run:

```bash
docker run --privileged --rm $(docker build -q --target test .)
```

# Debugging

## Debugging the tests
If you wish to debug the test code itself, that can be acheived by running:

```bash
docker run --privileged --rm -p 40000:40000 $(docker build -q --target debug .)
```

This will result in the tests running under dlv.  Connecting your debugger to localhost:40000 will allow you to debug.

```bash
-p 40000:40000
```
forwards port 40000 in the container to localhost:40000 where you can attach with your debugger.

```bash
--target debug
```

Runs the debug target, which is just like the test target, but starts tests with dlv listening on port 40000 inside the container.

## Debugging the cmd

When you run 'cmd' you will see an early line of output that tells you:

```Setting env variable DLV_LISTEN_FORWARDER to a valid dlv '--listen' value will cause the dlv debugger to execute this binary and listen as directed.```

If you follow those instructions when running the Docker container:
```bash
docker run --privileged -e DLV_LISTEN_FORWARDER=:50000 -p 50000:50000 --rm $(docker build -q --target test .)
```

```-e DLV_LISTEN_FORWARDER=:50000``` tells docker to set the environment variable DLV_LISTEN_FORWARDER to :50000 telling
dlv to listen on port 50000.

```-p 50000:50000``` tells docker to forward port 50000 in the container to port 50000 in the host.  From there, you can
just connect dlv using your favorite IDE and debug cmd.

## Debugging the tests and the cmd

```bash
docker run --privileged -e DLV_LISTEN_FORWARDER=:50000 -p 40000:40000 -p 50000:50000 --rm $(docker build -q --target debug .)
```

Please note, the tests **start** the cmd, so until you connect to port 40000 with your debugger and walk the tests
through to the point of running cmd, you will not be able to attach a debugger on port 50000 to the cmd.
