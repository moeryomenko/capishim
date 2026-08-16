# Vendored upstream content

This directory vendors content from upstream
[kubernetes-sigs/cluster-api](https://github.com/kubernetes-sigs/cluster-api)
at a pinned release tag. Every file in this directory is copied verbatim from
the upstream tag and must never be hand-edited. Changes happen deliberately via
`make vendor-templates` after bumping the upstream pin.

## Pin

- Upstream tag: `v1.14.0`
- Upstream commit: `560d4acf507bc7cac34b2da449fa5cd53eaeb149`
- kustomize version: `v5.6.0` (invoked as `go run sigs.k8s.io/kustomize/kustomize/v5@v5.6.0 build`)
- Vendored at: 2026-08-16T04:46:49Z (UTC)

## In-memory templates (REQ-007)

| File | Upstream source (at pin) |
|---|---|
| `cluster-template-in-memory.yaml` | `test/infrastructure/docker/templates/cluster-template-in-memory.yaml` |
| `clusterclass-in-memory.yaml` | `test/infrastructure/docker/templates/clusterclass-in-memory.yaml` |

The ClusterClass file is self-contained: every referenced template
(DevClusterTemplate, KubeadmControlPlaneTemplate, DevMachineTemplate,
KubeadmConfigTemplate) is defined inline in the same file, so exactly these two
files are vendored.

## Rendered provider manifests

Each `provider.yaml` is the full output of `kustomize build` over the
upstream provider's `config/default` (namespace + namePrefix kustomization),
including Deployment, Service, ServiceAccount, and cert-manager objects. The
setup container (`internal/manifests`) filters to the kinds it applies
(Namespace, CustomResourceDefinition, ClusterRole, ClusterRoleBinding, Role,
RoleBinding, ValidatingWebhookConfiguration, MutatingWebhookConfiguration).

| Provider file | Upstream kustomization source (at pin) | kustomization |
|---|---|---|
| `manifests/core/provider.yaml` | `core/config/default` | namespace `capi-system`, namePrefix `capi-` |
| `manifests/cabpk/provider.yaml` | `bootstrap/kubeadm/config/default` | namespace `capi-kubeadm-bootstrap-system`, namePrefix `capi-kubeadm-bootstrap-` |
| `manifests/kcp/provider.yaml` | `controlplane/kubeadm/config/default` | namespace `capi-kubeadm-control-plane-system`, namePrefix `capi-kubeadm-control-plane-` |
| `manifests/capd/provider.yaml` | `test/infrastructure/docker/config/default` | namespace `capd-system`, namePrefix `capd-` |

## Rules

- Do not hand-edit any file in this directory; upstream content is vendored verbatim.
- Pin consistency is enforced by `make check-pins` (REQ-013, VC-10).