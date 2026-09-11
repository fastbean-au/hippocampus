# Builds one of the two object-storage agents. The gateway fronts a bucket and reinforces the
# pointer-memory behind every object it serves; the reaper deletes the object behind a memory
# Hippocampus has forgotten. Which one is built is selected by the COMMAND build-arg, since the two
# differ only in their entry point:
#
#   docker build -f integrations/objectstore/Dockerfile --build-arg COMMAND=object-gateway \
#     -t hippocampus-object-gateway .
#
# The build context must be the repository root: integrations/objectstore is a separate Go module
# whose go.mod has `replace github.com/fastbean-au/hippocampus => ../..`, so the root module (the
# contract and observability packages it imports) must be present in the context.

# Build on the native BUILDPLATFORM and cross-compile to TARGETOS/TARGETARCH (CGO off), so a
# multi-arch build compiles each target natively rather than emulating the toolchain under QEMU.
# TARGETOS/TARGETARCH come from buildx and must be re-declared as ARGs in this stage to be referenced.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

ARG TARGETOS
ARG TARGETARCH

# COMMAND selects the binary: object-gateway or object-reaper.
ARG COMMAND=object-gateway

# VERSION (a git tag when building a release, "dev" otherwise) is stamped into the binary via
# -ldflags -X main.version so --version reports the release tag, matching the other tools.
ARG VERSION=dev

WORKDIR /src
COPY . .

# The replace directive in the module's go.mod resolves against /src, so the build must run from the
# module directory. CGO is off so the binary runs on a minimal base image with no C library.
WORKDIR /src/integrations/objectstore
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /hippocampus-agent "./cmd/${COMMAND}"

FROM alpine:3.24

ARG COMMAND=object-gateway
ARG VERSION=dev

LABEL org.opencontainers.image.title="hippocampus-${COMMAND}" \
    org.opencontainers.image.version="${VERSION}" \
    org.opencontainers.image.source="https://github.com/fastbean-au/hippocampus"

# Both agents listen: the gateway serves objects on 8088 and probes on 8090, the reaper receives
# callbacks on 8089 and probes on 8091. The ports are documented rather than published, since which
# of the two this image is depends on a build-arg.
EXPOSE 8088 8089 8090 8091

RUN adduser -D -H -u 1000 hippocampus

COPY --from=build /hippocampus-agent /usr/local/bin/hippocampus-agent

USER hippocampus

# No default CMD: --bucket is required, and the reaper additionally has to be told whether it may
# actually delete anything.
ENTRYPOINT ["hippocampus-agent"]
