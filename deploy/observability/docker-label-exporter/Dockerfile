# Multi-stage build, mirroring this repo's own root Dockerfile (README task
# 13.3) — same CGO_ENABLED=0/GOFLAGS=-trimpath discipline for a fully
# static binary. NOT distroless nonroot, unlike nexusd/signerd: this
# process's entire job is dialing /var/run/docker.sock, which is root:root
# mode 660 on a standard Docker install (confirmed on the machine this was
# built on) — a nonroot UID cannot connect to it regardless of image base,
# so there is no security benefit to pretending otherwise here. The real
# access-control boundary is the read-only socket mount itself
# (docker-compose.observability.yml's own comment on that).
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN go build -o /out/docker-label-exporter .

FROM gcr.io/distroless/static-debian12:latest
COPY --from=builder /out/docker-label-exporter /usr/local/bin/docker-label-exporter
EXPOSE 9101
ENTRYPOINT ["/usr/local/bin/docker-label-exporter"]
