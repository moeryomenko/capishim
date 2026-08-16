# syntax=docker/dockerfile:1.4

# capishim-setup -- initialization container for the capishim stack: generates
# the pod CA/certs, installs CRDs and RBAC, rewrites webhook configs, and emits
# the admin kubeconfig (REQ-002..REQ-005). Built from this module; the vendored
# in-memory templates and rendered provider manifests are baked in at
# /templates so the setup subcommands can read them.

# Provenance of the vendored templates/manifests (REQ-011/REQ-013): recorded in
# the final image label. Required at build time so provenance is never blank.
ARG CAPI_SOURCE_REF
# Global GOARCH so the runtime stage's FROM tag can use it.
ARG GOARCH=amd64

FROM golang:1.26-bookworm AS builder

ARG CAPI_SOURCE_REF
# Target architecture for the setup binary (also selects the distroless
# runtime tag).
ARG GOARCH=amd64

RUN test -n "${CAPI_SOURCE_REF}" \
    || { echo "error: CAPI_SOURCE_REF build arg is required (REQ-011)"; exit 1; }

WORKDIR /src

# The whole module (go.mod/go.sum, cmd/, internal/) is the build input.
COPY . .

RUN --mount=type=cache,id=capishim-setup-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=capishim-setup-gobuild-${GOARCH},target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH} \
    go build -trimpath -o /capishim ./cmd/capishim

FROM gcr.io/distroless/static:nonroot-${GOARCH} AS runtime

ARG CAPI_SOURCE_REF

LABEL io.capishim.capi-source-ref="${CAPI_SOURCE_REF}"

COPY --from=builder /capishim /capishim

# Bake the vendored in-memory templates and the rendered provider manifests at
# a known path (REQ-003, REQ-007, TASK-015 AC).
COPY templates/ /templates/

# No USER directive: the setup container writes to the shared state volume, and
# the quadlet unit / e2e driver owns the uid for that mount.
ENTRYPOINT ["/capishim"]
