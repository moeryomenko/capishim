# Vendored hypervisor provider manifests

The three hypervisor provider manifest trees under `templates/manifests/` are
vendored verbatim from the
[cluster-api-hypervisor](https://github.com/moeryomenko/cluster-api-hypervisor)
(CAPH) repository and must never be hand-edited. The setup container loads them
alongside the four stock provider trees (`core`, `cabpk`, `kcp`, `capd`) and
installs their CRDs, RBAC, and webhook configurations into the management
apiserver (REQ-001, REQ-002).

## Provenance

| File | Source (CAPH checkout) | CAPH commit | Copy date |
|---|---|---|---|
| `manifests/infrastructure-hypervisor/provider.yaml` | `out/infrastructure-hypervisor/v0.1.0/infrastructure-components.yaml` (`make components`) | `138ecba882e4d69ff55f9f51935bd199accc7b95` | 2026-08-23 |
| `manifests/bootstrap-hypervisor/provider.yaml` | `out/bootstrap-hypervisor/v0.1.0/bootstrap-components.yaml` (`make components`) | `138ecba882e4d69ff55f9f51935bd199accc7b95` | 2026-08-23 |
| `manifests/control-plane-hypervisor/provider.yaml` | `out/control-plane-hypervisor/v0.1.0/control-plane-components.yaml` (`make components`) | `138ecba882e4d69ff55f9f51935bd199accc7b95` | 2026-08-23 |

Release version: `v0.1.0` (CAPH `RELEASE_VERSION`).

All three files carry CAPH's single shared object set and are byte-identical:
the five hypervisor CRDs (`hypervisorclusters`,
`hypervisormachines`, `hypervisormachinetemplates` in
`infrastructure.cluster.x-k8s.io`; `hypervisorconfigs` in
`bootstrap.cluster.x-k8s.io`; `hypervisorcontrolplanes` in
`controlplane.cluster.x-k8s.io`), the `hypervisor-system` Namespace,
ServiceAccount, ClusterRole, ClusterRoleBinding, and the mutating and
validating webhook configurations with all ten webhook clientConfigs in URL
form (`https://127.0.0.1:9443/<path>`). There are no Deployment or Service
objects: the manager runs outside the capishim pod.

## Refreshing

From a checkout of this repository with a sibling CAPH checkout:

```sh
hack/update-hypervisor-manifests.sh [path-to-caph-checkout]
```

The script defaults to `../cluster-api-hypervisor`, runs `make components`
there, copies the three component files into the vendored trees, and fails
loudly if any source file is missing or empty. Re-running against an unchanged
CAPH checkout is a no-op (byte-identical output).

After refreshing, update the commit hash and copy date in the table above.
