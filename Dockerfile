# All three storage drivers (modernc.org/sqlite, jackc/pgx, and go-sql-driver/mysql) are pure
# Go, so the binary builds with CGO disabled and runs on a minimal base image with no C library.

FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /hippocampus ./cmd/hippocampus

# Alpine rather than scratch/distroless: busybox wget enables the compose healthcheck against the
# gateway's /healthz, which a shell-less image could not run.
FROM alpine:3.22

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
