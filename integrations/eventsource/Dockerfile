# Builds one event-sourcing broker bridge image. The bridge consumes from a message broker and
# stores each message in a running Hippocampus instance as a memory. Which broker is selected by the
# BROKER build-arg (nats | mqtt | rabbitmq | kafka | bluesky), so one Dockerfile produces all five
# images.
#
# The build context must be the repository root: integrations/eventsource is a separate Go module
# whose go.mod has `replace github.com/fastbean-au/hippocampus => ../..`, so the root module (the
# contract package it imports) must be present in the context. Build from the repo root with:
#
#   docker build -f integrations/eventsource/Dockerfile --build-arg BROKER=nats \
#     -t hippocampus-nats-bridge .

# Build on the native BUILDPLATFORM and cross-compile to TARGETOS/TARGETARCH (CGO off), so a
# multi-arch build compiles each target natively rather than emulating the toolchain under QEMU.
# TARGETOS/TARGETARCH come from buildx and must be re-declared as ARGs in this stage to be referenced.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS
ARG TARGETARCH

# Which broker bridge to build. Required - there is no sensible default.
ARG BROKER

# VERSION (a git tag when building a release, "dev" otherwise) is stamped into the binary via
# -ldflags -X main.version so --version reports the release tag, matching the other tools.
ARG VERSION=dev

WORKDIR /src
COPY . .

# The replace directive in the module's go.mod resolves against /src, so the build must run from the
# module directory. CGO is off so the binary runs on a minimal base image with no C library.
WORKDIR /src/integrations/eventsource
RUN test -n "${BROKER}" || (echo "BROKER build-arg is required (nats|mqtt|rabbitmq|kafka|bluesky)" && exit 1)
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /hippocampus-bridge "./cmd/${BROKER}"

# A bridge is primarily an outbound client: it dials the broker and the Hippocampus service. Since
# the instrumentation landed it also serves /healthz and /readyz on --health-port (8090, 0 disables),
# so the absence of an EXPOSE is a default rather than a property of the design - publish that port
# if you want to probe it. There is no useful default CMD: each broker requires its own flags
# (--subject/--topic/--queue, or none at all for bluesky), passed after the image name.
FROM alpine:3.24

# checkov:skip=CKV_DOCKER_2: a bridge exposes no port or health endpoint; liveness is left to the
# orchestrator (mirrors the root Dockerfile's mcp stage and the otel-collector image).
ARG VERSION=dev
ARG BROKER
LABEL org.opencontainers.image.title="hippocampus-${BROKER}-bridge" \
    org.opencontainers.image.version="${VERSION}" \
    org.opencontainers.image.source="https://github.com/fastbean-au/hippocampus"

RUN adduser -D -H -u 1000 hippocampus

COPY --from=build /hippocampus-bridge /usr/local/bin/hippocampus-bridge

USER hippocampus

ENTRYPOINT ["hippocampus-bridge"]
