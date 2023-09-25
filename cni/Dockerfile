ARG ARCH
ARG DROPGZ_VERSION=v0.0.12
ARG OS

FROM --platform=linux/${ARCH} mcr.microsoft.com/oss/go/microsoft/golang:1.21 AS azure-vnet
ARG DROPGZ_VERISON=$DROPGZ_VERSION
ARG OS
ARG VERSION
COPY . /go/src/azure-container-networking
WORKDIR /go/src/azure-container-networking
RUN GOOS=$OS CGO_ENABLED=0 go build -a -o /go/bin/azure-vnet -trimpath -ldflags "-X main.version="$VERSION"" -gcflags="-dwarflocationlists=true" cni/network/plugin/main.go
RUN GOOS=$OS CGO_ENABLED=0 go build -a -o /go/bin/azure-vnet-telemetry -trimpath -ldflags "-X main.version="$VERSION"" -gcflags="-dwarflocationlists=true" cni/telemetry/service/telemetrymain.go
RUN GOOS=$OS CGO_ENABLED=0 go build -a -o /go/bin/azure-vnet-ipam -trimpath -ldflags "-X main.version="$VERSION"" -gcflags="-dwarflocationlists=true" cni/ipam/plugin/main.go

RUN go mod download github.com/azure/azure-container-networking/dropgz@$DROPGZ_VERSION
WORKDIR /go/pkg/mod/github.com/azure/azure-container-networking/dropgz\@$DROPGZ_VERSION
COPY /go/bin/* pkg/embed/fs
COPY /go/src/azure-container-networking/cni/azure-$OS.conflist pkg/embed/fs/azure.conflist
COPY /go/src/azure-container-networking/cni/azure-$OS-swift.conflist pkg/embed/fs/azure-swift.conflist
COPY /go/src/azure-container-networking/cni/azure-$OS-swift-overlay.conflist pkg/embed/fs/azure-swift-overlay.conflist
COPY /go/src/azure-container-networking/cni/azure-$OS-swift-overlay-dualstack.conflist pkg/embed/fs/azure-swift-overlay-dualstack.conflist
RUN cd pkg/embed/fs/ && sha256sum * > sum.txt
RUN gzip --verbose --best --recursive pkg/embed/fs && for f in pkg/embed/fs/*.gz; do mv -- "$f" "${f%%.gz}"; done
RUN GOOS=$OS CGO_ENABLED=0 go build -a -o /go/bin/dropgz -trimpath -ldflags "-X github.com/Azure/azure-container-networking/dropgz/internal/buildinfo.Version="$VERSION"" -gcflags="-dwarflocationlists=true" main.go

FROM 
