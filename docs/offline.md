# Offline installation

An air-gapped host needs the images the quadlet units reference transferred
once, then [install.md](install.md) works unchanged: at boot nothing is pulled
from a registry because every `Image=` reference in the rendered units is
already present in the local podman store.

## Image inventory

The rendered quadlet units reference exactly seven images:

| Unit `Image=` reference | Origin |
|---|---|
| `localhost/capishim-setup:v0.1.0` | built by `make images` (used by both the `pki` and `setup` containers) |
| `localhost/capishim-core:v0.1.0` | built by `make images` |
| `localhost/capishim-cabpk:v0.1.0` | built by `make images` |
| `localhost/capishim-kcp:v0.1.0` | built by `make images` |
| `localhost/capishim-capd:v0.1.0` | built by `make images` |
| `registry.k8s.io/etcd:3.5.17-0` | pinned stock image (etcd v3.5.x) |
| `registry.k8s.io/kube-apiserver:v1.36.1` | pinned stock image |

The five built images compile the setup binary and the four provider manager
binaries from upstream cluster-api at the pinned `CAPI_SOURCE_REF` tag (the
build clones upstream, so the build machine needs network access). There is no
separate `capishim-pki` image: the `pki` container reuses
`localhost/capishim-setup:v0.1.0`.

The two control-plane images keep their stock `registry.k8s.io` names in the
quadlet units — they are pulled by `make images` but not re-tagged for the
pod. (`make images` additionally creates `localhost/capishim-etcd:v0.1.0` and
`localhost/capishim-apiserver:v0.1.0` aliases; those are used by the e2e
driver only, not by the quadlet units, so they are not needed on the offline
host.)

Tags follow `CAPISHIM_VERSION` for the built images (default `v0.1.0`); if you
build with a different version, use that tag in the commands below.

## On the networked build machine

```sh
make images
```

Verify the set (optional):

```sh
podman images --format '{{.Repository}}:{{.Tag}}' | grep -E '^(localhost/capishim-|registry.k8s.io/etcd|registry.k8s.io/kube-apiserver)'
```

Export all seven images into one archive:

```sh
podman save -o capishim-images.tar \
  localhost/capishim-setup:v0.1.0 \
  localhost/capishim-core:v0.1.0 \
  localhost/capishim-cabpk:v0.1.0 \
  localhost/capishim-kcp:v0.1.0 \
  localhost/capishim-capd:v0.1.0 \
  registry.k8s.io/etcd:3.5.17-0 \
  registry.k8s.io/kube-apiserver:v1.36.1
```

## Transfer

Copy `capishim-images.tar` to the offline host (USB drive, `scp`, your
preferred medium). Also transfer a checkout of the capishim repository (or
the built binary plus the rendered quadlet units and `templates/`) — see
below.

## On the offline host

```sh
podman load -i capishim-images.tar
```

Verify the images are present:

```sh
podman images --format '{{.Repository}}:{{.Tag}}' | grep -E '^(localhost/capishim-|registry.k8s.io/etcd|registry.k8s.io/kube-apiserver)'
```

Then install and boot exactly as in [install.md](install.md):

```sh
make install-quadlet && systemctl --user daemon-reload && systemctl --user start capishim-pod
KUBECONFIG=~/.kube/capishim.kubeconfig kubectl get nodes
```

Note: `make install-quadlet` builds the `capishim` renderer binary with
`go build` and copies `templates/` into the state directory, so the offline
host needs the Go toolchain (Go 1.26) and the repository checkout, or a
pre-staged state directory and pre-rendered units. No image pulls happen at
install or boot time.

If the offline host must run without the Go toolchain, pre-render and
pre-stage everything on the build machine:

1. `make install-quadlet` once on the build machine with the target state
   directory (`CAPISHIM_STATE_DIR=...`),
2. copy the rendered units from `~/.config/containers/systemd/` and the
   populated state directory (including `templates/`) to the offline host,
3. on the offline host: `systemctl --user daemon-reload && systemctl --user
   start capishim-pod`.

## Updating the offline host

Rebuild on the networked machine, re-export, re-load, then
`make install-quadlet && systemctl --user daemon-reload && systemctl --user
restart capishim-pod` — state in `${CAPISHIM_STATE_DIR}` is preserved.
