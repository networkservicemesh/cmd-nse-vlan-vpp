ARG VPP_VERSION=v24.10.0-4-ga9d527a67
FROM ghcr.io/networkservicemesh/govpp/vpp:${VPP_VERSION} as go
COPY --from=golang:1.23.8 /usr/local/go/ /go
ENV PATH ${PATH}:/go/bin
ENV GO111MODULE=on
ENV CGO_ENABLED=0
ENV GOBIN=/bin
ARG BUILDARCH=amd64
RUN rm -r /etc/vpp
RUN go install github.com/go-delve/delve/cmd/dlv@v1.22.0
RUN go install github.com/grpc-ecosystem/grpc-health-probe@v0.4.22
ADD https://github.com/spiffe/spire/releases/download/v1.8.7/spire-1.8.7-linux-${BUILDARCH}-musl.tar.gz .
RUN tar xzvf spire-1.8.7-linux-${BUILDARCH}-musl.tar.gz -C /bin --strip=2 spire-1.8.7/bin/spire-server spire-1.8.7/bin/spire-agent

FROM go as build
WORKDIR /build
COPY go.mod go.sum ./
COPY . .
RUN go build -o /bin/nse-vlan-vpp .

FROM build as test
CMD go test -test.v ./...

FROM test as debug
CMD dlv -l :40000 --headless=true --api-version=2 test -test.v ./...

FROM ghcr.io/networkservicemesh/govpp/vpp:${VPP_VERSION} as runtime
COPY --from=build /bin/nse-vlan-vpp /bin/nse-vlan-vpp
ENTRYPOINT ["/bin/nse-vlan-vpp"]
