# All three storage drivers (modernc.org/sqlite, jackc/pgx, and go-sql-driver/mysql) are pure
# Go, so the binary builds with CGO disabled and runs on a minimal base image with no C library.

FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

# VERSION (a git tag when building a release, "dev" otherwise) is stamped into the binary via
# -ldflags -X so --version and /healthz report the release tag, not "(devel)". A working-tree build
# never picks the tag up through debug.BuildInfo.Main.Version, so it must be injected explicitly.
ARG VERSION=dev

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.buildVersion=${VERSION}" \
    -o /hippocampus ./cmd/hippocampus

# The MCP bridge (cmd/hippocampus-mcp) is a separate binary and a separate image (the `mcp` stage
# below). Its version var is main.version, not main.buildVersion.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /hippocampus-mcp ./cmd/hippocampus-mcp

# The MCP-server image. Selected with `target: mcp` (see the profile-gated `mcp` service in
# docker-compose.yaml); it runs the streamable-HTTP transport so an MCP host can reach it over the
# network. The stdio transport is not useful in a container - for local stdio use, build the binary
# on the host and point your MCP host at the service's exposed gRPC port (see docs/mcp.md). No
# HEALTHCHECK: the streamable-HTTP handler exposes no health endpoint. It is built before the
# default hippocampus stage so a plain (no-target) `docker compose build` still selects hippocampus.
FROM alpine:3.22 AS mcp

ARG VERSION=dev
LABEL org.opencontainers.image.title="hippocampus-mcp" \
    org.opencontainers.image.version="${VERSION}" \
    org.opencontainers.image.source="https://github.com/fastbean-au/hippocampus"

RUN adduser -D -H -u 1000 hippocampus

COPY --from=build /hippocampus-mcp /usr/local/bin/hippocampus-mcp

USER hippocampus

# Streamable-HTTP transport default (the compose service sets the address/transport flags).
EXPOSE 8090

ENTRYPOINT ["hippocampus-mcp"]

# Alpine rather than scratch/distroless: busybox wget enables the compose healthcheck against the
# gateway's /healthz, which a shell-less image could not run.
FROM alpine:3.22 AS hippocampus

# Build identification. Pass --build-arg VERSION=<tag> to stamp a release; the running binary also
# reports its embedded module/VCS version via --version and the /healthz body.
ARG VERSION=dev
LABEL org.opencontainers.image.title="hippocampus" \
    org.opencontainers.image.version="${VERSION}" \
    org.opencontainers.image.source="https://github.com/fastbean-au/hippocampus"

RUN adduser -D -H -u 1000 hippocampus \
    && mkdir -p /data /etc/hippocampus \
    && chown hippocampus /data

COPY --from=build /hippocampus /usr/local/bin/hippocampus
COPY docker/config.sqlite.json /etc/hippocampus/config.json

USER hippocampus

# 50051 gRPC, 8080 HTTP/JSON gateway (also serves /healthz)
EXPOSE 50051 8080

# The SQLite database lives here; mount a volume to persist it. The postgres compose file
# overrides the config instead and needs no data volume on this container.
VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["wget", "-q", "--spider", "http://localhost:8080/readyz"]
# /readyz is database-aware (returns 503 if the store is unreachable); /healthz is pure process
# liveness for orchestrators that must not restart on a transient dependency outage.

ENTRYPOINT ["hippocampus"]
CMD ["-c", "/etc/hippocampus/config.json"]
