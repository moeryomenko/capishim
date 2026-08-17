# System-level install (escape hatch)

By default capishim installs as user-scope quadlet units under
`~/.config/containers/systemd/` and publishes the apiserver on the loopback
address `127.0.0.1:6443`. For a host where the stack must survive logout
without lingering, or where remote clients need access to the management
apiserver, install at system scope and publish on all interfaces.

## Prerequisites

Everything in [install.md](install.md), plus:

- **root** — the system-scope quadlet directory `/etc/containers/systemd/`
  is root-owned;
- **a systemd system session** with podman quadlet support (rootful podman).

The system-level install runs the same `hack/install-quadlet.sh` script with
`CAPISHIM_SYSTEM=1`, which switches the install directory to
`/etc/containers/systemd/`, uses `systemctl` (system scope) instead of
`systemctl --user`, and skips the linger step (not needed for system units).
The script requires root and fails with a clear message otherwise:
`CAPISHIM_SYSTEM=1 installs into /etc/containers/systemd/ and requires root`.

## Install

```sh
sudo CAPISHIM_SYSTEM=1 make install-quadlet
```

The script builds the `capishim` renderer with `go build`, so the Go
toolchain must be reachable under `sudo` (use `sudo env "PATH=$PATH" ...` if
your Go install is not on root's PATH). The install:

1. renders the nine quadlet units for the current configuration;
2. installs them into `/etc/containers/systemd/`;
3. runs `systemctl daemon-reload` (system scope);
4. copies `templates/` into `${CAPISHIM_STATE_DIR}/templates/`;
5. symlinks `~/.kube/capishim.kubeconfig` to
   `${CAPISHIM_STATE_DIR}/kubeconfigs/admin.kubeconfig`.

`CAPISHIM_STATE_DIR` defaults to `$HOME/.local/share/capishim`, where `$HOME`
is root's home under `sudo`. For a fixed location, set it explicitly, e.g.:

```sh
sudo CAPISHIM_SYSTEM=1 CAPISHIM_STATE_DIR=/var/lib/capishim make install-quadlet
```

## Publishing on all interfaces

The management apiserver publishes on `127.0.0.1:6443` by default. To expose
it beyond the loopback (e.g. for remote `kubectl`/`clusterctl` clients):

```sh
sudo CAPISHIM_SYSTEM=1 CAPISHIM_BIND_ADDRESS=0.0.0.0:6443 make install-quadlet
```

`CAPISHIM_BIND_ADDRESS` is the host part of the pod's `PublishPort`
(`0.0.0.0:6443:6443` in this case); the container port stays fixed at 6443.
The apiserver inside the pod always binds `0.0.0.0:6443` regardless of this
setting — the override only controls the host-side publish address.

The generated admin kubeconfig always points at `https://127.0.0.1:6443`
(even with a wildcard bind address), so remote clients must rewrite the
`server` field in their copy of the kubeconfig to the host's reachable
address, e.g. `https://<host>:6443`. The kubeconfig trusts the pod CA, so
client cert auth still works unchanged.

## Start and verify

```sh
sudo systemctl daemon-reload
sudo systemctl start capishim-pod
```

```sh
sudo systemctl status capishim-pod
KUBECONFIG=~/.kube/capishim.kubeconfig kubectl get nodes
```

The pod unit is `capishim.pod`; systemd exposes it as
`capishim-pod.service`. Boot order and behavior are identical to the user
install (`pki -> etcd -> apiserver -> setup -> managers`; oneshot `pki` and
`setup` units; no leader-election flags; `--feature-gates=ClusterTopology=true`
on all four managers; `User=0` for the capishim-built containers;
`POD_IP=127.0.0.1` for capd). With `CAPISHIM_BIND_ADDRESS=0.0.0.0:6443`, a
remote client can reach the management apiserver on any host interface.

## Restart and updates

```sh
sudo systemctl restart capishim-pod
```

State persists in `${CAPISHIM_STATE_DIR}` across restarts and reboots — etcd
data, the pod CA, and the kubeconfigs live on the host volume. Re-running
`sudo CAPISHIM_SYSTEM=1 make install-quadlet` is idempotent and refreshes the
installed units, the template copy, and the kubeconfig symlink.

## Back to user scope

Remove the system units and reinstall at user scope:

```sh
sudo systemctl stop capishim-pod
sudo rm -f /etc/containers/systemd/capishim.pod /etc/containers/systemd/capishim-*.container
sudo systemctl daemon-reload
make install-quadlet && systemctl --user daemon-reload && systemctl --user start capishim-pod
```

State in `${CAPISHIM_STATE_DIR}` is shared and preserved across the switch.
