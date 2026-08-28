// Package shim implements the e2e bootstrap provider for the capishim
// management stack: it drives podman directly (no systemd, no kind, no
// clusterctl init) to create and manage the capishim pod that the ginkgo
// suite in this module uses as its management cluster (REQ-012, VC-01..VC-08,
// plan assumption 6).
//
// The component table below deliberately duplicates the component data owned
// by github.com/moeryomenko/capishim/internal/config and the quadlet renderer
// (github.com/moeryomenko/capishim/internal/quadlet): this module is a
// separate Go module (its own go.mod) and Go's internal-package rule forbids
// importing internal/ from outside the owning module, so the table is
// replicated here as constants. The duplication is intentional and documented
// here so the two tables can be kept in sync deliberately.
//
// Runtime corrections the e2e driver applies on top of the quadlet contract
// (the quadlet units themselves do not yet carry these; they are the
// responsibility of the quadlet/systemd path and are tracked for alignment):
//
//   - Every capishim-built image except capd runs as uid 65532 (distroless
//     nonroot). Rootless podman maps that uid outside the host user's subuid
//     range, so the pki/setup containers cannot write the host-owned state
//     directory; the driver forces --user 0 so container writes land in the
//     host user's uid (the state dir stays host-readable for assertions and
//     cert minting).
//   - etcd reads its TLS certificates from <state>/pki, so the etcd container
//     additionally mounts <state>/pki read-only (the quadlet etcd unit only
//     mounts <state>/etcd).
//   - The provider managers' kubeconfigs reference <state>/pki/<id>-manager.crt,
//     <state>/pki/<id>-manager.key and <state>/pki/ca.crt, so each manager
//     mounts <state>/pki read-only in addition to the kubeconfigs and webhook
//     cert directories.
//   - The four managers share one pod network namespace; their default
//     health-probe and diagnostics listeners both collide on :9440 / :8443,
//     so the driver assigns per-manager ports.
//   - CAPI v1.14 manager binaries accept no --leader-election-namespace flag,
//     and controller-runtime fails to start leader election when it runs
//     outside a cluster without a LeaderElectionNamespace; the drivers omit
//     --leader-elect entirely (single-instance managers need no election).
//   - kube-apiserver v1.36 removed --authorization-rbac-super-user, and the
//     setup container must bootstrap RBAC from a clean cluster (it applies
//     Namespaces before the admin ClusterRoleBinding exists), so the
//     apiserver authorizes the admin identity via a bootstrap ABAC rule while
//     every other identity is governed by RBAC. This is what lets VC-03's
//     unbound-identity Forbidden check pass while the admin can bootstrap.
//   - kube-apiserver binds 0.0.0.0 inside the pod: rootless podman's port
//     publishing forwards host traffic to the pod IP, not to the pod's
//     loopback, so a loopback-only bind is unreachable from the host.
package shim

// Pod name and container name prefix. Container names are "capishim-<id>";
// VC-01/VC-03 assert exactly these names.
const (
	// PodName is the podman pod name of the management stack.
	PodName = "capishim"

	// containerPrefix prefixes every container name in the pod.
	containerPrefix = PodName + "-"

	// apiserverContainerPort is the apiserver port inside the pod; the pod
	// always publishes it regardless of the host bind address (REQ-010).
	apiserverContainerPort = "6443"

	// defaultBindAddress is the loopback address the apiserver publishes on
	// (REQ-010), overridable via CAPISHIM_BIND_ADDRESS.
	defaultBindAddress = "127.0.0.1:6443"

	// etcdClientPort is the TLS client port etcd listens on inside the pod.
	etcdClientPort = "2379"

	// etcdPeerPort is the (unused in single-node mode) TLS peer port.
	etcdPeerPort = "2380"

	// envStateDir and envBindAddress are the environment keys the
	// capishim-built binaries consume via config.Load.
	envStateDir    = "CAPISHIM_STATE_DIR"
	envBindAddress = "CAPISHIM_BIND_ADDRESS"

	// envPreserve keeps the state directory on Dispose when set.
	envPreserve = "CAPISHIM_E2E_PRESERVE"

	// setupImage is shared by the pki and setup oneshot containers; the
	// manager images are localhost/capishim-<id>:v0.1.0.
	setupImage = "localhost/capishim-setup:v0.1.0"

	// etcdImage and apiserverImage are the re-tagged stock control-plane
	// images produced by make images (REQ-011).
	etcdImage      = "localhost/capishim-etcd:v0.1.0"
	apiserverImage = "localhost/capishim-apiserver:v0.1.0"

	// abacPolicyPath is the state-relative path of the bootstrap ABAC policy
	// file mounted into the apiserver.
	abacPolicyPath = "abac/policy.json"
)

// volume describes one bind mount of a container.
type volume struct {
	// hostPath is the state-directory path on the host, also used as the
	// mount point inside the container (the quadlet units mount state paths
	// at the same absolute path, REQ-009).
	hostPath string
	// readOnly marks the mount :ro.
	readOnly bool
}

// component describes one container of the capishim pod.
type component struct {
	// id is the component identifier; the container name is capishim-<id>.
	id string
	// image is the container image reference.
	image string
	// volumes are the bind mounts, in the order they are passed to podman.
	volumes []volume
	// env are KEY=VALUE environment pairs.
	env []string
	// command is appended to the image entrypoint.
	command []string
	// user forces the container uid; empty keeps the image default.
	user string
}

// name returns the container name for the component.
func (c component) name() string {
	return containerPrefix + c.id
}

// components returns all eight pod components in boot order: pki, etcd,
// apiserver, setup, then the four provider managers.
func components(stateDir string) []component {
	return []component{
		{
			id:      "pki",
			image:   setupImage,
			user:    "0",
			volumes: []volume{{hostPath: stateDir + "/pki"}, {hostPath: stateDir + "/webhook-certs"}},
			env:     []string{envStateDir + "=" + stateDir},
			command: []string{"pki"},
		},
		{
			id:    "etcd",
			image: etcdImage,
			volumes: []volume{
				{hostPath: stateDir + "/etcd"},
				{hostPath: stateDir + "/pki", readOnly: true},
			},
			command: []string{
				"etcd",
				"--name=" + PodName + "-etcd",
				"--data-dir=" + stateDir + "/etcd",
				"--listen-client-urls=https://127.0.0.1:" + etcdClientPort,
				"--advertise-client-urls=https://127.0.0.1:" + etcdClientPort,
				"--listen-peer-urls=https://127.0.0.1:" + etcdPeerPort,
				"--initial-advertise-peer-urls=https://127.0.0.1:" + etcdPeerPort,
				"--initial-cluster=" + PodName + "-etcd=https://127.0.0.1:" + etcdPeerPort,
				"--cert-file=" + stateDir + "/pki/etcd-server.crt",
				"--key-file=" + stateDir + "/pki/etcd-server.key",
				"--trusted-ca-file=" + stateDir + "/pki/ca.crt",
				"--client-cert-auth=true",
				"--peer-cert-file=" + stateDir + "/pki/etcd-server.crt",
				"--peer-key-file=" + stateDir + "/pki/etcd-server.key",
				"--peer-trusted-ca-file=" + stateDir + "/pki/ca.crt",
				"--peer-client-cert-auth=true",
			},
		},
		{
			id:    "apiserver",
			image: apiserverImage,
			volumes: []volume{
				{hostPath: stateDir + "/pki", readOnly: true},
				{hostPath: stateDir + "/abac", readOnly: true},
			},
			command: []string{
				"kube-apiserver",
				"--etcd-servers=https://127.0.0.1:" + etcdClientPort,
				"--etcd-cafile=" + stateDir + "/pki/ca.crt",
				"--etcd-certfile=" + stateDir + "/pki/apiserver-client.crt",
				"--etcd-keyfile=" + stateDir + "/pki/apiserver-client.key",
				"--client-ca-file=" + stateDir + "/pki/ca.crt",
				"--tls-cert-file=" + stateDir + "/pki/apiserver.crt",
				"--tls-private-key-file=" + stateDir + "/pki/apiserver.key",
				"--service-account-key-file=" + stateDir + "/pki/sa.pub",
				"--service-account-signing-key-file=" + stateDir + "/pki/sa.key",
				"--service-account-issuer=https://127.0.0.1:" + apiserverContainerPort,
				"--authorization-mode=ABAC,RBAC",
				"--authorization-policy-file=" + stateDir + "/" + abacPolicyPath,
				"--bind-address=0.0.0.0",
				"--secure-port=" + apiserverContainerPort,
				"--service-cluster-ip-range=10.128.0.0/12",
				"--allow-privileged=true",
			},
		},
		{
			id:    "setup",
			image: setupImage,
			user:  "0",
			volumes: []volume{
				{hostPath: stateDir + "/pki", readOnly: true},
				{hostPath: stateDir + "/webhook-certs"},
				{hostPath: stateDir + "/kubeconfigs"},
			},
			env: []string{
				envStateDir + "=" + stateDir,
				envBindAddress + "=" + "127.0.0.1:" + apiserverContainerPort,
			},
			command: []string{"setup"},
		},
		managerComponent("core", "capi-system", 9443, 9451, 8451, stateDir),
		managerComponent("cabpk", "capi-kubeadm-bootstrap-system", 9444, 9452, 8452, stateDir),
		managerComponent("kcp", "capi-kubeadm-control-plane-system", 9445, 9453, 8453, stateDir),
		managerComponent("capd", "capd-system", 9446, 9454, 8454, stateDir),
	}
}

// managerComponent builds the spec for one provider manager container.
//
// The manager binaries are the stock upstream v1.14 binaries (entrypoint
// /manager), so the driver passes every flag on the command line, mirroring
// the quadlet Command= lines (REQ-006) with the e2e corrections documented in
// the package comment: per-manager health/diagnostics ports (the defaults
// collide inside the shared pod network namespace), no leader-election flags
// (the v1.14 binaries reject --leader-election-namespace and controller-runtime
// cannot default the election namespace outside a cluster), and the shared
// <state>/pki read-only mount their kubeconfigs require.
func managerComponent(
	id, providerNamespace string,
	webhookPort, healthPort, diagnosticsPort int,
	stateDir string,
) component {
	c := component{
		id:    id,
		image: "localhost/capishim-" + id + ":v0.1.0",
		user:  "0",
		volumes: []volume{
			{hostPath: stateDir + "/pki", readOnly: true},
			{hostPath: stateDir + "/kubeconfigs", readOnly: true},
			{hostPath: stateDir + "/pki/" + id + "-webhook", readOnly: true},
		},
		command: []string{
			"--kubeconfig=" + stateDir + "/kubeconfigs/" + id + ".kubeconfig",
			"--webhook-port=" + itoa(webhookPort),
			"--webhook-cert-dir=" + stateDir + "/pki/" + id + "-webhook",
			"--health-addr=127.0.0.1:" + itoa(healthPort),
			"--diagnostics-address=127.0.0.1:" + itoa(diagnosticsPort),
			// Every manager must see ClusterTopology enabled: the core and
			// control-plane webhooks gate topology fields on the gate, and the
			// infrastructure provider's DevClusterTemplate/DevMachineTemplate
			// validation rejects topology-shaped objects when it is off
			// (REQ-006, VC-05/VC-07).
			"--feature-gates=ClusterTopology=true",
		},
	}
	if id == "capd" {
		// The CAPD in-memory backend serves every fake workload cluster's
		// apiserver on a per-cluster port of the workload-clusters mux. The
		// mux host comes from the POD_IP env var and becomes the workload
		// ControlPlaneEndpoint host (and the server of the generated
		// <cluster>-kubeconfig Secrets). All CAPI managers run in the same
		// pod network namespace, so the loopback address is reachable and its
		// SAN is present on the workload apiserver certificates (REQ-008).
		c.env = []string{"POD_IP=127.0.0.1"}
	}
	return c
}

// itoa converts a small integer to its decimal string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
