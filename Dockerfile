# Multi-stage build for nexusd and signerd (README task 13.3, closing
# production-readiness finding F3: "there is no way to package or deploy the
# binary"). Migrations are already embedded (migrations/embed.go), so the
# nexusd image is self-contained with no extra COPY for them.
#
# Build one target at a time:
#   docker build --target nexusd  -t nexus-agent-demo/nexusd  .
#   docker build --target signerd -t nexus-agent-demo/signerd .

FROM golang:1.25 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: a fully static binary, required for a distroless "static"
# base (no libc). GOFLAGS=-trimpath keeps build-machine paths out of the
# binary.
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN go build -o /out/nexusd  ./cmd/nexusd
RUN go build -o /out/signerd ./cmd/signerd

# Pre-create the directories a named volume mounts onto in the final images
# (deploy/docker-compose.yml's app profile: .dev/ for a dev-generated KEK/
# AuthN key, /var/run/nexus for the signerd unix socket both containers
# share), owned by 65532:65532 — distroless's own "nonroot" uid/gid. Docker
# initializes a FRESH named volume from whatever the image already has at
# its mount path, so this is what keeps that volume writable by nonroot
# instead of defaulting to root:root.
RUN mkdir -p /writable/app/.dev /writable/var/run/nexus && chown -R 65532:65532 /writable

# --- nexusd ---
FROM gcr.io/distroless/static-debian12:nonroot AS nexusd
COPY --from=builder /out/nexusd /usr/local/bin/nexusd
COPY --from=builder --chown=65532:65532 /writable/app /app
COPY --from=builder --chown=65532:65532 /writable/var/run/nexus /var/run/nexus
WORKDIR /app
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/nexusd"]

# --- signerd ---
FROM gcr.io/distroless/static-debian12:nonroot AS signerd
COPY --from=builder /out/signerd /usr/local/bin/signerd
COPY --from=builder --chown=65532:65532 /writable/app /app
COPY --from=builder --chown=65532:65532 /writable/var/run/nexus /var/run/nexus
WORKDIR /app
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/signerd"]
