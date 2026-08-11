# Builds the ingestor image. The ingestor promotes completed events from an edge ("source")
# Hippocampus instance into a central ("target") one, under a CEL rules file that decides, per
# event, whether the data is worth keeping at all.
#
# The build context must be the repository root: integrations/ingestor is a separate Go module whose
# go.mod has `replace github.com/fastbean-au/hippocampus => ../..`, so the root module (the contract
# package it imports) must be present in the context. Build from the repo root with:
#
#   docker build -f integrations/ingestor/Dockerfile -t hippocampus-ingestor .

# Build on the native BUILDPLATFORM and cross-compile to TARGETOS/TARGETARCH (CGO off), so a
# multi-arch build compiles each target natively rather than emulating the toolchain under QEMU.
# TARGETOS/TARGETARCH come from buildx and must be re-declared as ARGs in this stage to be referenced.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

ARG TARGETOS
ARG TARGETARCH

# VERSION (a git tag when building a release, "dev" otherwise) is stamped into the binary via
# -ldflags -X main.version so --version reports the release tag, matching the other tools.
ARG VERSION=dev

WORKDIR /src
COPY . .

# The replace directive in the module's go.mod resolves against /src, so the build must run from the
# module directory. CGO is off so the binary runs on a minimal base image with no C library.
WORKDIR /src/integrations/ingestor
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /hippocampus-ingestor ./cmd/ingestor

# The ingestor is an outbound client only: it dials two Hippocampus instances and listens on no port,
# so there is no EXPOSE and no HEALTHCHECK (nothing to probe). It also has no useful default CMD -
# --rules and --target-address are required, and the rules file must be mounted in.
FROM alpine:3.22

# checkov:skip=CKV_DOCKER_2: the ingestor exposes no port or health endpoint; liveness is left to the
# orchestrator (mirrors the eventsource bridge images and the root Dockerfile's mcp stage).
ARG VERSION=dev
LABEL org.opencontainers.image.title="hippocampus-ingestor" \
    org.opencontainers.image.version="${VERSION}" \
    org.opencontainers.image.source="https://github.com/fastbean-au/hippocampus"

RUN adduser -D -H -u 1000 hippocampus

COPY --from=build /hippocampus-ingestor /usr/local/bin/hippocampus-ingestor

USER hippocampus

ENTRYPOINT ["hippocampus-ingestor"]
