# syntax=docker/dockerfile:1.4

# capishim-capd -- Docker infrastructure provider (CAPD) manager built from the
# pinned upstream cluster-api tag (REQ-011). CAPD lives in the separate
# upstream test/ module, so the build mirrors the upstream
# test/infrastructure/docker/Dockerfile: the full upstream tree is cloned and
# the manager is compiled from /workspace/test/infrastructure/docker. The
# pinned tag is recorded in the final image label (REQ-011 PASS).

# The pinned upstream tag. Required at build time (no default): the build
# fails loudly if it is missing or names a tag that does not exist.
ARG CAPI_SOURCE_REF
# Global GOARCH so the runtime stage's FROM tag can use it.
ARG GOARCH=amd64

FROM golang:1.26-bookworm AS builder

ARG CAPI_SOURCE_REF
# Target architecture for the manager binary (also selects the distroless
# runtime tag).
ARG GOARCH=amd64

RUN test -n "${CAPI_SOURCE_REF}" \
    || { echo "error: CAPI_SOURCE_REF build arg is required (REQ-011)"; exit 1; }

# The upstream clone is cached across builds with a BuildKit cache mount keyed
# by the pinned tag; the Go module and compile caches are cached separately.
# The clone is skipped on rebuilds when the cached checkout is already present.
RUN --mount=type=cache,id=capishim-src-${CAPI_SOURCE_REF},target=/capisrc \
    if [ ! -d /capisrc/cluster-api/.git ]; then \
        rm -rf /capisrc/cluster-api; \
        git clone --depth 1 --branch "${CAPI_SOURCE_REF}" \
            https://github.com/kubernetes-sigs/cluster-api.git \
            /capisrc/cluster-api; \
    fi; \
    cp -a /capisrc/cluster-api /workspace

# The test/ module replaces the root and api modules with local paths, so the
# full tree must be present before downloading its dependencies (mirrors the
# upstream CAPD Dockerfile, which copies go.mod/go.sum for root, api, and test
# before `go mod download` in /workspace/test).
WORKDIR /workspace/test

RUN --mount=type=cache,id=capishim-gomod-${CAPI_SOURCE_REF},target=/go/pkg/mod \
    go mod download

# Build the CAPD manager. The upstream Dockerfile uses -a here; it is dropped
# so the shared go-build cache can skip up-to-date packages.
WORKDIR /workspace/test/infrastructure/docker

RUN --mount=type=cache,id=capishim-gomod-${CAPI_SOURCE_REF},target=/go/pkg/mod \
    --mount=type=cache,id=capishim-gobuild-${GOARCH},target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH} \
    go build -trimpath -o /manager .

# CAPD intentionally does not use nonroot: upstream notes it cannot, because
# docker requires access to the docker socket (in-memory mode does not need
# either, but the image stays drop-in compatible with upstream's).
FROM gcr.io/distroless/static:latest-${GOARCH} AS runtime

ARG CAPI_SOURCE_REF

LABEL io.capishim.capi-source-ref="${CAPI_SOURCE_REF}"

COPY --from=builder /manager /manager

ENTRYPOINT ["/manager"]
