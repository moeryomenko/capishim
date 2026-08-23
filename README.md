# capishim

capishim replaces a stock Cluster API (CAPI) management cluster with a
self-contained, systemd-managed **podman quadlet pod**. The pod runs etcd,
kube-apiserver, and the four CAPI provider manager binaries (core, kubeadm
bootstrap, kubeadm control-plane, CAPD) as per-component containers, plus a Go
setup container that performs initialization: CA/cert generation, CRD
installation, RBAC provisioning, webhook configuration rewrite, and admin
kubeconfig emission.

Workload clusters are provisioned through CAPD's **in-memory backend** — no
Docker socket, no real kubelet. This is an ops-ergonomics replacement for
edge/dev/CI environments: one `systemctl --user` unit graph boots the whole
management stack, state persists across restarts and reboots, and per-component
restart and log inspection work through systemd.

## How it works

A stock `clusterctl init` installs provider controllers as Deployments, which
requires kube-controller-manager, a scheduler, kubelet, and a container
runtime. capishim avoids that by running the manager **binaries** directly as
pod containers and performing the initialization itself (CRD install, RBAC,
webhook wiring). The result is a management apiserver that speaks the full CAPI
contract — `Cluster` reaches `Provisioned`, `KubeadmControlPlane` reports
`Initialized`, `Machine`s report `Ready` — against in-memory fake workload
clusters.

Upstream cluster-api is consumed strictly as an **external dependency pinned to
a released tag** (default `v1.14.0`). Provider manager binaries are built from
that tag at image-build time, the in-memory templates and provider CRD/RBAC
manifests are vendored from it, and the e2e suite imports its public test
framework. No code in this project is contributed back upstream.

## Repository layout

```
capishim/
├── go.mod
├── Makefile                        # images, install-quadlet, vendor-templates, check-pins, verify
├── cmd/capishim/                   # setup container entrypoint binary
├── internal/
│   ├── pki/                        # CA + cert generation
│   ├── manifests/                  # CRD + RBAC install from vendored upstream YAML
│   ├── webhookrewrite/             # webhook config rewrite
│   ├── quadlet/                    # quadlet unit file rendering
│   └── config/                     # defaults: state dir, ports, bind address
├── templates/                      # vendored from upstream @ pinned tag (+ VENDORED.md, manifests/)
├── quadlet/                        # quadlet unit sources rendered by internal/quadlet
├── images/                         # Containerfiles per component
├── e2e/                            # ginkgo suite (VC-01..VC-08)
├── hack/                           # install-quadlet.sh, vendor.sh, check-pins.sh
└── docs/                           # install, usage, offline, system-level
```

## Prerequisites

- **podman >= 4.7** — the quadlet units rely on `Requires=`/`After=`,
  `PublishPort`, and pod-level env handling.
- **Go 1.26** — `make images` builds the provider/setup binaries;
  `make install-quadlet` builds the `capishim` renderer with `go build`.
- **systemd with an active user session** — the stack installs as user units
  under `~/.config/containers/systemd/`.
- **loginctl** (systemd-logind) — `make install-quadlet` enables lingering so
  user units keep running after logout (best-effort).
- **kubectl and clusterctl** — for talking to the management apiserver and the
  template-based provisioning flow.
- **A rootless, container-enabled host** — podman with `subuid`/`subgid`
  entries for your user.

No registry credentials, no cluster-api release binaries, and no Docker socket
are required.

## Quick start

From the repository root:

```sh
make images && make install-quadlet && systemctl --user daemon-reload && systemctl --user start capishim-pod
```

- `make images` builds the five capishim images
  (`localhost/capishim-{setup,core,cabpk,kcp,capd}:v0.1.0`) from
  `images/*.Containerfile`, cloning upstream cluster-api at the pinned
  `CAPI_SOURCE_REF`, and pulls the two pinned stock control-plane images
  (`registry.k8s.io/etcd:3.5.17-0`, `registry.k8s.io/kube-apiserver:v1.36.1`).
- `make install-quadlet` renders the nine quadlet units
  (`capishim.pod` plus `capishim-{pki,etcd,apiserver,setup,core,cabpk,kcp,capd}.container`),
  installs them into `~/.config/containers/systemd/`, enables lingering, copies
  `templates/` into the state directory, and symlinks
  `~/.kube/capishim.kubeconfig`.
- `systemctl --user start capishim-pod` boots the whole stack in dependency
  order: `pki -> etcd -> apiserver -> setup -> the four managers`.

Verify the boot:

```sh
systemctl --user status capishim-pod
KUBECONFIG=~/.kube/capishim.kubeconfig kubectl get nodes
```

`kubectl get nodes` prints an empty table (`No resources found`) — the
management apiserver has no node objects, and that empty response is the
expected healthy result.

## Provisioning an in-memory cluster

```sh
# Apply the ClusterClass first (creates DevClusterTemplate, KCP template, etc.)
KUBECONFIG=~/.kube/capishim.kubeconfig \
  kubectl apply -f ~/.local/share/capishim/templates/clusterclass-in-memory.yaml

# Generate and apply a ClusterClass-based cluster
KUBECONFIG=~/.kube/capishim.kubeconfig clusterctl generate cluster demo \
  --from ~/.local/share/capishim/templates/cluster-template-in-memory.yaml \
  --kubernetes-version v1.36.1 \
  --control-plane-machine-count 3 --worker-machine-count 3 \
  | KUBECONFIG=~/.kube/capishim.kubeconfig kubectl apply -f -
```

Watch it reconcile to `Provisioned`:

```sh
KUBECONFIG=~/.kube/capishim.kubeconfig kubectl get cluster demo
KUBECONFIG=~/.kube/capishim.kubeconfig kubectl get kubeadmcontrolplane
KUBECONFIG=~/.kube/capishim.kubeconfig kubectl get machine
```

The in-memory cluster has no real kubelet — it exists to exercise the CAPI
reconciliation contract, not to run real workloads. See
[docs/usage.md](docs/usage.md) for scaling, restart/reboot semantics, and the
RBAC model.

## Make targets

| Target | Description |
|---|---|
| `make images` | Build all container images (providers from the pinned tag). |
| `make install-quadlet` | Render and install the quadlet units into the systemd user dir. |
| `make vendor-templates` | Re-vendor the in-memory templates from the pinned upstream tag. |
| `make check-pins` | Verify upstream pin consistency (go.mod, Containerfiles, provenance). |
| `make test-e2e-shim` | Run the e2e suite against the quadlet pod management cluster. |
| `make verify-shim` | Full verification flow (VC-01..VC-10). |
| `make check` | Lint + vet + unit tests (CI gate). |
| `make test` / `make cover` | Run unit tests / open the coverage report. |
| `make build` | Build the `capishim` setup binary. |
| `make lint` / `make fmt` / `make vet` | Code quality targets. |

## Configuration

All overrides are optional; the defaults work for a single-user host.

| Variable | Default | Effect |
|---|---|---|
| `CAPISHIM_VERSION` | `v0.1.0` | Image tag for the `localhost/capishim-*` images. |
| `CAPISHIM_STATE_DIR` | `~/.local/share/capishim` | State directory: etcd data, pki material, kubeconfigs, ABAC policy, vendored templates. |
| `CAPISHIM_BIND_ADDRESS` | `127.0.0.1:6443` | Host address the apiserver port is published on. |
| `CAPI_SOURCE_REF` | `v1.14.0` | Upstream cluster-api tag the provider images are built from. |

## Verification

```sh
make verify-shim
```

runs the complete verification contract: `make check` (VC-09),
`make check-pins` (VC-10), and `make test-e2e-shim` (VC-01..VC-08). The e2e
suite drives podman directly (no systemd) and asserts the full contract —
clean-host bootstrap, setup idempotency, CRD + RBAC, webhook rewrite,
in-memory cluster reaching `Provisioned`, restart persistence, and
ClusterClass + MachinePool. It takes roughly 10–15 minutes.

## Upstream pinning

The upstream cluster-api tag is centralized in three places that must agree:
`CAPI_SOURCE_REF` in the Makefile/Containerfiles, the framework import in
`e2e/go.mod`, and the provenance recorded in `templates/VENDORED.md`.
`make check-pins` fails if they diverge. Upgrades are deliberate and gated on
the e2e contract passing.

## Non-objectives

- No real workload clusters: no Docker socket, no real kubelet, no CNI.
- No `clusterctl init` / `clusterctl move`: the shim performs its own init.
- No Runtime SDK / runtime extensions (`RuntimeSDK` gate stays off).
- CAPD in-memory is the only infrastructure provider.

## Documentation

- [docs/install.md](docs/install.md) — fresh user-scope install, boot, and troubleshooting.
- [docs/usage.md](docs/usage.md) — provisioning, scaling, restart/reboot, RBAC.
- [docs/offline.md](docs/offline.md) — air-gapped distribution via `podman save`/`load`.
- [docs/system-level.md](docs/system-level.md) — system-scope escape hatch and wildcard bind.

## License

Dual-licensed under [Apache 2.0](LICENSE-APACHE) and [MIT](LICENSE-MIT).
