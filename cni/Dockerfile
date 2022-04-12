FROM docker.io/golang:1.18 AS builder
ARG VERSION
WORKDIR /azure-container-networking
COPY . .
RUN CGO_ENABLED=0 go build -a -o bin/azure-vnet -ldflags "-X main.version="$VERSION"" -gcflags="-dwarflocationlists=true" cni/network/plugin/main.go
RUN sha256sum bin/* > bin/sum.txt
RUN mv bin/* cni/mgr/pkg/embed/bin && gzip --best --recursive cni/mgr/pkg/embed/bin && for f in cni/mgr/pkg/embed/bin/*.gz; do mv -- "$f" "${f%%.gz}"; done
RUN CGO_ENABLED=0 go build -a -o bin/mgr -ldflags "-X github.com/Azure/azure-container-networking/cni/mgr/internal/buildinfo.Version="$VERSION"" -gcflags="-dwarflocationlists=true" cni/mgr/main.go

FROM scratch
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group
COPY --from=builder azure-container-networking/bin/mgr /usr/local/bin/mgr
ENTRYPOINT [ "/usr/local/bin/mgr" ]
