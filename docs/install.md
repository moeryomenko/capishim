# Installing capishim

capishim replaces a stock Cluster API management cluster with a
systemd-managed podman quadlet pod: etcd, kube-apiserver, and the four CAPI
provider manager binaries (core, kubeadm bootstrap, kubeadm control-plane,
CAPD) run as per-component containers, and a Go setup container performs the
initialization (CA/certs, CRDs, RBAC, webhook rewrite, admin kubeconfig).
Workload clusters are provisioned through CAPD's in-memory backend — no
Docker socket, no real kubelet.

This page covers a fresh, user-scope install on a single host. The
system-level escape hatch is documented in [system-level.md](system-level.md),
air-gapped distribution in [offline.md](offline.md).

## Prerequisites

- **podman >= 4.7** — the quadlet units rely on `Requires=`/`After=`,
  `PublishPort`, and pod-level env handling from podman 4.7 and newer.
- **Go 1.26** — `make images` builds the capishim and provider binaries, and
  `make install-quadlet` builds the `capishim` renderer binary with `go build`.
- **systemd with an active user session** — the stack is installed as user
  units under `~/.config/containers/systemd/`.
- **loginctl** (systemd-logind) — `make install-quadlet` enables lingering for
  your uid so user units keep running after logout. This step is best-effort:
  if `loginctl` is missing the install still succeeds, but the pod stops when
  you log out unless you enable linger yourself.
- **kubectl and clusterctl** — `kubectl` is needed to talk to the management
  apiserver; `clusterctl` is needed for the template-based provisioning flow
  in [usage.md](usage.md).
- **A container-enabled host** — rootless podman with `subuid`/`subgid`
  entries for your user (standard on Fedora, Arch, Ubuntu 22.04+; see the
  podman rootless setup docs for your distribution).

No registry credentials, no cluster-api release binaries, and no Docker
socket are required.

## Build and install

From the repository root:

```sh
make images && make install-quadlet && systemctl --user daemon-reload && systemctl --user start capishim-pod
```

### What each step does

1. `make images` — builds the five capishim images
   (`localhost/capishim-{setup,core,cabpk,kcp,capd}:v0.1.0`) from
   `images/*.Containerfile`, cloning upstream cluster-api at the pinned
   `CAPI_SOURCE_REF` (default `v1.14.0`) at build time, and pulls the two
   pinned stock control-plane images (`registry.k8s.io/etcd:3.5.17-0`,
   `registry.k8s.io/kube-apiserver:v1.36.1`). The rendered quadlet units
   reference the stock images verbatim and the five localhost images; the
   `capishim-setup` image is shared by the `pki` and `setup` containers, so
   the pod needs exactly seven image references and there is no separate
   `capishim-pki` image. (`make images` also creates
   `localhost/capishim-etcd:v0.1.0` and `localhost/capishim-apiserver:v0.1.0`
   aliases; those are consumed by the e2e driver, not by the quadlet units.)

2. `make install-quadlet` — runs `hack/install-quadlet.sh`, which:
   - builds the `capishim` renderer and renders the nine quadlet units
     (`capishim.pod` plus `capishim-{pki,etcd,apiserver,setup,core,cabpk,kcp,capd}.container`)
     for the current configuration (state dir, bind address, image version);
   - installs them into `~/.config/containers/systemd/` (mode 0644);
   - runs `systemctl --user daemon-reload`;
   - enables lingering for your uid (best-effort, user mode only);
   - copies `templates/` into `${CAPISHIM_STATE_DIR}/templates/` for
     `clusterctl generate cluster --from` (REQ-007);
   - symlinks `~/.kube/capishim.kubeconfig` to
     `${CAPISHIM_STATE_DIR}/kubeconfigs/admin.kubeconfig`, which the setup
     container writes at boot (REQ-010).

   The script is idempotent: re-running it overwrites the installed units with
   freshly rendered ones, refreshes the template copy, and re-points the
   kubeconfig symlink.

3. `systemctl --user daemon-reload` — makes systemd re-read the quadlet
   directory. (install-quadlet already ran this, but it is harmless to run it
   again as part of the one-liner.)

4. `systemctl --user start capishim-pod` — starts the whole stack. The pod
   unit is `capishim.pod`; systemd exposes it as `capishim-pod.service`.
   Boot order is encoded in the units: `pki -> etcd -> apiserver -> setup ->
   the four managers`. The `pki` and `setup` containers are oneshot units
   (`Type=oneshot`, `RemainAfterExit=yes`) built from the `capishim-setup`
   image, whose `/capishim` entrypoint implements the initialization
   subcommands (`pki` for certificate generation, `setup` for CRD/RBAC/
   webhook/kubeconfig setup); etcd, the apiserver, and the managers run
   long-lived. The e2e suite exercises the same container contract by
   invoking those subcommands directly against podman (see
   [usage.md](usage.md)).

## Environment overrides

All of these are optional; the defaults work for a single-user host.

| Variable | Default | Effect |
|---|---|---|
| `CAPISHIM_VERSION` | `v0.1.0` | Image tag for the `localhost/capishim-*` images. |
| `CAPISHIM_STATE_DIR` | `~/.local/share/capishim` | State directory: etcd data, pki material, kubeconfigs, ABAC policy, vendored templates. |
| `CAPISHIM_BIND_ADDRESS` | `127.0.0.1:6443` | Host address the apiserver port is published on. |
| `CAPI_SOURCE_REF` | `v1.14.0` | Upstream cluster-api tag the provider images are built from (`make images`). |

Example with overrides:

```sh
CAPISHIM_VERSION=v0.1.0 CAPISHIM_STATE_DIR=/var/lib/capishim CAPISHIM_BIND_ADDRESS=0.0.0.0:6443 make install-quadlet
```

## Verifying the boot

```sh
systemctl --user status capishim-pod
KUBECONFIG=~/.kube/capishim.kubeconfig kubectl get nodes
```

`kubectl get nodes` succeeds once the setup container has finished writing the
admin kubeconfig (it targets the symlink target
`~/.local/share/capishim/kubeconfigs/admin.kubeconfig`). The management
apiserver has no node objects, so the command prints an empty `NAME` table
with `No resources found` — that is the expected healthy response. For a
deeper check:

```sh
KUBECONFIG=~/.kube/capishim.kubeconfig kubectl get clusters
KUBECONFIG=~/.kube/capishim.kubeconfig kubectl get clusterclass
```

`kubectl get clusterclass` works because every manager runs with
`--feature-gates=ClusterTopology=true`.

## What runs inside the pod

The pod shares one network namespace. Ports inside the pod:

| Container | Image | Pod-internal listeners |
|---|---|---|
| `capishim-pki` | `localhost/capishim-setup:v0.1.0` | oneshot (certs) |
| `capishim-etcd` | `registry.k8s.io/etcd:3.5.17-0` | TLS client `127.0.0.1:2379`, TLS peer `127.0.0.1:2380` |
| `capishim-apiserver` | `registry.k8s.io/kube-apiserver:v1.36.1` | `0.0.0.0:6443` |
| `capishim-setup` | `localhost/capishim-setup:v0.1.0` | oneshot (CRDs, RBAC, webhooks, kubeconfigs) |
| `capishim-core` | `localhost/capishim-core:v0.1.0` | webhook `9443`, health `127.0.0.1:9451`, diagnostics `127.0.0.1:8451` |
| `capishim-cabpk` | `localhost/capishim-cabpk:v0.1.0` | webhook `9444`, health `127.0.0.1:9452`, diagnostics `127.0.0.1:8452` |
| `capishim-kcp` | `localhost/capishim-kcp:v0.1.0` | webhook `9445`, health `127.0.0.1:9453`, diagnostics `127.0.0.1:8453` |
| `capishim-capd` | `localhost/capishim-capd:v0.1.0` | webhook `9446`, health `127.0.0.1:9454`, diagnostics `127.0.0.1:8454` |

Behavior that the installed units implement (all verified by the e2e suite):

- **Feature gates**: all four managers run with
  `--feature-gates=ClusterTopology=true`; bootstrap and infrastructure
  webhooks gate topology fields, so the gate must be on everywhere.
- **No leader election**: the manager Exec lines carry no
  `--leader-elect`/`--leader-election-namespace` flags. The v1.14 binaries
  reject `--leader-election-namespace`, and controller-runtime cannot default
  an election namespace outside a cluster; the single-instance managers need
  no election.
- **apiserver**: binds `0.0.0.0:6443` inside the pod (rootless podman port
  publishing forwards host traffic to the pod IP, not pod loopback) and
  authorizes with `--authorization-mode=ABAC,RBAC` plus the bootstrap ABAC
  policy file in the state dir (`abac/policy.json`); the setup container
  bootstraps RBAC from a clean cluster.
- **etcd**: single-node TLS cluster on loopback; client and peer traffic are
  both TLS-authenticated with the pod CA.
- **User=0**: the capishim-built containers run as `User=0`. Their images are
  distroless with default uid 65532, which cannot write a host-owned state
  directory under rootless podman.
- **CAPD**: the capd container sets `Environment=POD_IP=127.0.0.1`; the
  in-memory backend mux host becomes the workload `ControlPlaneEndpoint`.

## Inspecting and troubleshooting

```sh
systemctl --user list-units 'capishim-*'
journalctl --user -u capishim-pod
journalctl --user -u capishim-setup.service
podman ps --pod
```

To tear the stack down (state is preserved):

```sh
systemctl --user stop capishim-pod
```

## Next steps

Provision an in-memory workload cluster: see [usage.md](usage.md).
