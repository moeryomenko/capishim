package config

// ComponentID identifies one container in the shim pod.
type ComponentID string

// The eight components of the shim pod, in boot order: pki, etcd, apiserver,
// setup, then the four provider managers.
const (
	// ComponentPKI generates the pod CA, component certs, and SA signing keys.
	ComponentPKI ComponentID = "pki"
	// ComponentEtcd is the single-node etcd backing the management apiserver.
	ComponentEtcd ComponentID = "etcd"
	// ComponentAPIServer is the management Kubernetes apiserver.
	ComponentAPIServer ComponentID = "apiserver"
	// ComponentSetup installs CRDs, RBAC, webhook rewrites, and kubeconfigs.
	ComponentSetup ComponentID = "setup"
	// ComponentCore is the cluster-api core provider manager.
	ComponentCore ComponentID = "core"
	// ComponentCABPK is the kubeadm bootstrap provider manager.
	ComponentCABPK ComponentID = "cabpk"
	// ComponentKCP is the kubeadm control-plane provider manager.
	ComponentKCP ComponentID = "kcp"
	// ComponentCAPD is the Docker provider manager (in-memory backend).
	ComponentCAPD ComponentID = "capd"

	// Webhook listener ports for the four provider managers.
	webhookPortCore  = 9443
	webhookPortCABPK = 9444
	webhookPortKCP   = 9445
	webhookPortCAPD  = 9446
)

// ComponentSpec describes one container of the shim pod: its image, and for
// the four provider managers the webhook port, namespace, name prefix,
// manager certificate CN, and kubeconfig path.
type ComponentSpec struct {
	// ID is the component identifier, one of the Component* constants.
	ID ComponentID
	// Image is the container image reference.
	Image string
	// WebhookPort is the manager webhook listener port; zero for components
	// without a webhook.
	WebhookPort int
	// ProviderNamespace is the namespace the provider managers run in.
	ProviderNamespace string
	// NamePrefix prefixes provider-managed object names.
	NamePrefix string
	// ManagerCN is the certificate CN for the manager's client certificate.
	ManagerCN string
	// Kubeconfig is the manager kubeconfig path relative to the state
	// directory.
	Kubeconfig string
}

// Components returns all component specs in boot order (pki -> etcd ->
// apiserver -> setup -> providers). Each call returns a fresh slice, so
// mutating a returned spec does not affect later calls.
func Components() []ComponentSpec {
	return []ComponentSpec{
		{ID: ComponentPKI, Image: "localhost/capishim-setup:v0.1.0"},
		{ID: ComponentEtcd, Image: "registry.k8s.io/etcd:3.5.17-0"},
		{ID: ComponentAPIServer, Image: "registry.k8s.io/kube-apiserver:v1.36.1"},
		{ID: ComponentSetup, Image: "localhost/capishim-setup:v0.1.0"},
		{
			ID:                ComponentCore,
			Image:             "localhost/capishim-core:v0.1.0",
			WebhookPort:       webhookPortCore,
			ProviderNamespace: "capi-system",
			NamePrefix:        "capi-",
			ManagerCN:         "capishim:core-manager",
			Kubeconfig:        kubeconfigsDir + "/core.kubeconfig",
		},
		{
			ID:                ComponentCABPK,
			Image:             "localhost/capishim-cabpk:v0.1.0",
			WebhookPort:       webhookPortCABPK,
			ProviderNamespace: "capi-kubeadm-bootstrap-system",
			NamePrefix:        "capi-kubeadm-bootstrap-",
			ManagerCN:         "capishim:cabpk-manager",
			Kubeconfig:        kubeconfigsDir + "/cabpk.kubeconfig",
		},
		{
			ID:                ComponentKCP,
			Image:             "localhost/capishim-kcp:v0.1.0",
			WebhookPort:       webhookPortKCP,
			ProviderNamespace: "capi-kubeadm-control-plane-system",
			NamePrefix:        "capi-kubeadm-control-plane-",
			ManagerCN:         "capishim:kcp-manager",
			Kubeconfig:        kubeconfigsDir + "/kcp.kubeconfig",
		},
		{
			ID:                ComponentCAPD,
			Image:             "localhost/capishim-capd:v0.1.0",
			WebhookPort:       webhookPortCAPD,
			ProviderNamespace: "capd-system",
			NamePrefix:        "capd-",
			ManagerCN:         "capishim:capd-manager",
			Kubeconfig:        kubeconfigsDir + "/capd.kubeconfig",
		},
	}
}

// Component returns the spec for the given ID. The bool is false for unknown
// IDs.
func Component(id ComponentID) (ComponentSpec, bool) {
	for _, spec := range Components() {
		if spec.ID == id {
			return spec, true
		}
	}
	return ComponentSpec{}, false
}
