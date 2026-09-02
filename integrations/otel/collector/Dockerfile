# Builds an OpenTelemetry Collector that ships the Hippocampus logs exporter, assembled with the
# OpenTelemetry Collector Builder (OCB) from builder-config.yaml. The build context must be the
# repository root: the builder manifest's `replaces:` point at the sibling exporter module
# (integrations/otel/hippocampusexporter) and the repo-root contract module, so both must be present
# in the context. Build from the repo root with:
#
#   docker build -f integrations/otel/collector/Dockerfile -t hippocampus-otel-collector .

# Build on the native BUILDPLATFORM and cross-compile the collector to TARGETOS/TARGETARCH (CGO off),
# so a multi-arch build never emulates the OCB toolchain under QEMU. TARGETOS/TARGETARCH come from
# buildx and must be re-declared as ARGs in this stage to be referenced. OCB itself (go install
# below) is left native to the build host - only the collector it compiles is cross-targeted.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS
ARG TARGETARCH

# Must track otelcol_version in builder-config.yaml.
ARG OCB_VERSION=0.157.0

# git: the go toolchain needs it to resolve some collector-contrib module dependencies. The git
# version is left unpinned to track the alpine repo, matching the unpinned tooling used elsewhere.
# hadolint ignore=DL3018
RUN apk add --no-cache git \
    && go install go.opentelemetry.io/collector/cmd/builder@v${OCB_VERSION}

WORKDIR /src
COPY . .

# OCB generates sources under _build/ and compiles them; the relative replaces in the manifest
# resolve against that generated go.mod, so it must run from the collector directory. CGO is off so
# the binary runs on a minimal base image with no C library.
WORKDIR /src/integrations/otel/collector
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} builder --config builder-config.yaml

FROM alpine:3.24 AS otel-collector

# checkov:skip=CKV_DOCKER_2: the bundled config enables no health_check extension, so there is no
# endpoint to probe; liveness is left to the orchestrator (mirrors the root Dockerfile's mcp stage).
ARG VERSION=dev
LABEL org.opencontainers.image.title="hippocampus-otel-collector" \
    org.opencontainers.image.version="${VERSION}" \
    org.opencontainers.image.source="https://github.com/fastbean-au/hippocampus"

RUN adduser -D -H -u 1000 otel

COPY --from=build /src/integrations/otel/collector/_build/hippocampus-otelcol /usr/local/bin/hippocampus-otelcol
# The bundled config tails sample.log and ships to a local Hippocampus - a runnable demo default.
# Mount your own config over /etc/otelcol/config.yaml (and adjust the CMD) for a real deployment.
COPY integrations/otel/collector/config.yaml integrations/otel/collector/sample.log /etc/otelcol/

USER otel

# 4317 OTLP gRPC (the bundled config opens an otlp receiver alongside the filelog tail).
EXPOSE 4317

WORKDIR /etc/otelcol
ENTRYPOINT ["hippocampus-otelcol"]
CMD ["--config", "/etc/otelcol/config.yaml"]
