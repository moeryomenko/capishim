# syntax=docker/dockerfile:1.4

# capishim-cabpk -- kubeadm bootstrap (CABPK) controller manager built from the
# pinned upstream cluster-api tag (REQ-011). Built from
# bootstrap/kubeadm/main.go; the pinned tag is recorded in the final image
# label so built-from provenance is inspectable (REQ-011 PASS).

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

WORKDIR /workspace

RUN --mount=type=cache,id=capishim-gomod-${CAPI_SOURCE_REF},target=/go/pkg/mod \
    go mod download

# -tags=fieldsv1string mirrors the upstream root Dockerfile build (required by
# the pinned apimachinery for the fields.v1 string conversion workaround).
RUN --mount=type=cache,id=capishim-gomod-${CAPI_SOURCE_REF},target=/go/pkg/mod \
    --mount=type=cache,id=capishim-gobuild-${GOARCH},target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH} \
    go build -tags=fieldsv1string -trimpath \
    -ldflags "-extldflags '-static'" -o /manager ./bootstrap/kubeadm

FROM gcr.io/distroless/static:nonroot-${GOARCH} AS runtime

ARG CAPI_SOURCE_REF

LABEL io.capishim.capi-source-ref="${CAPI_SOURCE_REF}"

COPY --from=builder /manager /manager

# Numeric uid (65532) as kubernetes expects for pod security policies; mirrors
# the upstream manager images.
USER 65532
ENTRYPOINT ["/manager"]
