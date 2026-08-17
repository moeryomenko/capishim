package shim

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// ContainerState is one container's observed state inside the capishim pod.
// The State values match podman's ps states; the specs assert "running" for
// the long-running containers and "exited" (with ExitCode 0) for the pki and
// setup oneshots (VC-01).
type ContainerState struct {
	Name     string
	State    string
	ExitCode int
}

// ClusterProvider implements bootstrap.ClusterProvider for the capishim
// management stack, driving podman directly (plan assumption 6: no systemd in
// the e2e path). It owns the pod lifecycle (Create, Restart, Dispose), the
// state probes (ContainerStates, ManagerLogs), and the kubeconfig path
// (GetKubeconfigPath).
//
// The provider is not safe for concurrent use.
type ClusterProvider struct {
	// stateDir is the shared state directory backing the pod (REQ-009).
	stateDir string
	// bindAddress is the host apiserver bind address (REQ-010).
	bindAddress string
	// preserve keeps the state directory on Dispose when CAPISHIM_E2E_PRESERVE
	// is set.
	preserve bool
}

// NewClusterProvider returns a provider for the given state directory. The
// bind address defaults to 127.0.0.1:6443 and is overridable via
// CAPISHIM_BIND_ADDRESS; CAPISHIM_E2E_PRESERVE keeps the state directory on
// Dispose when set to any non-empty value.
func NewClusterProvider(stateDir string) *ClusterProvider {
	bind := os.Getenv(envBindAddress)
	if bind == "" {
		bind = defaultBindAddress
	}
	return &ClusterProvider{
		stateDir:    stateDir,
		bindAddress: bind,
		preserve:    os.Getenv(envPreserve) != "",
	}
}

// Timeouts bounding the boot sequence. VC-01 requires the apiserver to accept
// authenticated requests within five minutes.
const (
	// oneshotTimeout bounds podman wait for the pki and setup containers.
	oneshotTimeout = 2 * time.Minute
	// healthzTimeout bounds the apiserver /healthz wait (VC-01).
	healthzTimeout = 5 * time.Minute
	// healthzProbeTimeout is the per-request timeout of a single probe.
	healthzProbeTimeout = 5 * time.Second
	// healthzPollInterval is the delay between probes.
	healthzPollInterval = time.Second
	// runningTimeout bounds the wait for every long-running container to
	// report running after the managers start.
	runningTimeout = 2 * time.Minute
)

// Create boots the capishim pod on the current host: it removes any stale
// pod, creates the pod and all eight containers, then starts them in boot
// order (pki -> etcd -> apiserver -> setup -> managers), waiting for pki and
// setup to exit 0 and for the apiserver to answer /healthz (VC-01, REQ-012).
// Create has no error return (bootstrap.ClusterProvider); failures panic with
// the driver error so the ginkgo suite reports them as a BeforeSuite failure.
func (p *ClusterProvider) Create(ctx context.Context) {
	if err := p.create(ctx); err != nil {
		panic(fmt.Sprintf("shim: Create: %v", err))
	}
}

// create is the error-returning half of Create.
func (p *ClusterProvider) create(ctx context.Context) error {
	if err := p.ensureStateDirs(); err != nil {
		return err
	}
	if err := p.writeABACPolicy(); err != nil {
		return err
	}

	// A previous run may have left a pod or containers behind; remove them so
	// this boot starts from a clean slate (VC-01's "clean host" condition).
	runPodmanQuiet(ctx, "pod", "rm", "-f", PodName)
	for _, c := range components(p.stateDir) {
		runPodmanQuiet(ctx, "rm", "-f", c.name())
	}

	if _, err := runPodman(ctx, "pod", "create", "--name", PodName, "--publish", p.publishPort()); err != nil {
		return fmt.Errorf("create pod: %w", err)
	}
	for _, c := range components(p.stateDir) {
		if err := p.createContainer(ctx, c); err != nil {
			return err
		}
	}
	return p.boot(ctx)
}

// Restart stops and restarts the pod with the same readiness waits as Create
// (VC-02, VC-06): the pod CA and admin certificates persist on the host
// volume, so a restart must not regenerate them (REQ-002, REQ-009). Restart
// is synchronous and returns only when the stack is ready again.
func (p *ClusterProvider) Restart(ctx context.Context) {
	if err := p.restart(ctx); err != nil {
		panic(fmt.Sprintf("shim: Restart: %v", err))
	}
}

// restart is the error-returning half of Restart.
func (p *ClusterProvider) restart(ctx context.Context) error {
	runPodmanQuiet(ctx, "pod", "stop", PodName)
	return p.boot(ctx)
}

// GetKubeconfigPath returns the admin kubeconfig emitted by the setup
// container (stateDir/kubeconfigs/admin.kubeconfig).
func (p *ClusterProvider) GetKubeconfigPath() string {
	return filepath.Join(p.stateDir, "kubeconfigs", "admin.kubeconfig")
}

// Dispose removes the pod and all its containers, then resets the state
// directory unless CAPISHIM_E2E_PRESERVE is set. It is synchronous (the
// bootstrap.ClusterProvider contract) and best-effort: a pod that is already
// gone is not an error.
func (p *ClusterProvider) Dispose(ctx context.Context) {
	runPodmanQuiet(ctx, "pod", "rm", "-f", PodName)
	if !p.preserve {
		_ = os.RemoveAll(p.stateDir)
	}
}

// ContainerStates inspects every container in the capishim pod and returns
// its name, podman state, and exit code (VC-01).
func (p *ClusterProvider) ContainerStates(ctx context.Context) ([]ContainerState, error) {
	out, err := runPodman(ctx, "ps", "-a", "--filter", "pod="+PodName, "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("inspect pod containers: %w", err)
	}
	var list []struct {
		Names    []string `json:"Names"`
		State    string   `json:"State"`
		ExitCode int      `json:"ExitCode"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse podman ps output: %w", err)
	}
	states := make([]ContainerState, 0, len(list))
	for _, c := range list {
		for _, name := range c.Names {
			states = append(states, ContainerState{Name: name, State: c.State, ExitCode: c.ExitCode})
		}
	}
	return states, nil
}

// ManagerLogs returns the combined stdout/stderr logs of one container in the
// pod (VC-03 scans them for RBAC Forbidden errors).
func (p *ClusterProvider) ManagerLogs(ctx context.Context, container string) (string, error) {
	out, err := runPodman(ctx, "logs", container)
	if err != nil {
		return "", fmt.Errorf("read logs of %s: %w", container, err)
	}
	return out, nil
}

// publishPort renders the pod's --publish value: the bind address host, the
// host port, and the fixed container port (REQ-010). The host and container
// ports are the same apiserver port; podman's syntax is host:hostPort:containerPort.
func (p *ClusterProvider) publishPort() string {
	host := p.bindAddress
	if h, _, err := net.SplitHostPort(p.bindAddress); err == nil {
		host = h
	}
	return host + ":" + apiserverContainerPort + ":" + apiserverContainerPort
}

// apiserverPort extracts the port from the bind address (the setup container
// derives the client URL the same way).
func (p *ClusterProvider) apiserverPort() string {
	_, port, err := net.SplitHostPort(p.bindAddress)
	if err != nil {
		return apiserverContainerPort
	}
	return port
}

// ensureStateDirs creates the state subdirectories the containers mount and
// the e2e driver writes (the pki container recreates <state>/pki itself, but
// pre-creating every mount source keeps podman's bind-mount behavior uniform).
func (p *ClusterProvider) ensureStateDirs() error {
	dirs := []string{
		filepath.Join(p.stateDir, "pki"),
		filepath.Join(p.stateDir, "etcd"),
		filepath.Join(p.stateDir, "kubeconfigs"),
		filepath.Join(p.stateDir, "abac"),
	}
	for _, c := range components(p.stateDir) {
		if c.id == "apiserver" || c.id == "pki" || c.id == "setup" || c.id == "etcd" {
			continue
		}
		dirs = append(dirs, filepath.Join(p.stateDir, "pki", c.id+"-webhook"))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create state directory %s: %w", dir, err)
		}
	}
	return nil
}

// writeABACPolicy writes the bootstrap ABAC policy that grants the admin
// identity (capishim:admin) every action so the setup container can bootstrap
// CRDs, RBAC, and webhooks on a clean cluster; every other identity is
// governed by RBAC (REQ-004, VC-03).
func (p *ClusterProvider) writeABACPolicy() error {
	const policy = `{"apiVersion":"abac.authorization.kubernetes.io/v1beta1","kind":"Policy","spec":{"user":"capishim:admin","namespace":"*","resource":"*","apiGroup":"*","nonResourcePath":"*"}}` + "\n"
	path := filepath.Join(p.stateDir, abacPolicyPath)
	if err := os.WriteFile(path, []byte(policy), 0o644); err != nil {
		return fmt.Errorf("write ABAC policy %s: %w", path, err)
	}
	return nil
}

// createContainer creates one container in the pod with the component's
// image, mounts, environment, user, and command.
func (p *ClusterProvider) createContainer(ctx context.Context, c component) error {
	args := []string{"create", "--name", c.name(), "--pod", PodName, "--pull=never"}
	if c.user != "" {
		args = append(args, "--user", c.user)
	}
	for _, v := range c.volumes {
		mode := "rw"
		if v.readOnly {
			mode = "ro"
		}
		args = append(args, "--volume", v.hostPath+":"+v.hostPath+":"+mode)
	}
	for _, e := range c.env {
		args = append(args, "--env", e)
	}
	args = append(args, c.image)
	args = append(args, c.command...)
	if _, err := runPodman(ctx, args...); err != nil {
		return fmt.Errorf("create container %s: %w", c.name(), err)
	}
	return nil
}

// boot starts the containers in boot order and waits for readiness: pki and
// setup must exit 0, the apiserver must answer /healthz, and the six
// long-running containers must report running.
func (p *ClusterProvider) boot(ctx context.Context) error {
	if err := p.startOneshot(ctx, "pki"); err != nil {
		return err
	}
	if _, err := runPodman(ctx, "start", containerPrefix+"etcd"); err != nil {
		return fmt.Errorf("start etcd: %w", err)
	}
	if _, err := runPodman(ctx, "start", containerPrefix+"apiserver"); err != nil {
		return fmt.Errorf("start apiserver: %w", err)
	}
	if err := p.waitForHealthz(ctx); err != nil {
		return err
	}
	if err := p.startOneshot(ctx, "setup"); err != nil {
		return err
	}
	if err := p.ensureDirectManagerBindings(ctx); err != nil {
		return err
	}
	for _, id := range []string{"core", "cabpk", "kcp", "capd"} {
		if _, err := runPodman(ctx, "start", containerPrefix+id); err != nil {
			return fmt.Errorf("start manager %s: %w", id, err)
		}
	}
	return p.waitForRunning(ctx, containerPrefix+"etcd", containerPrefix+"apiserver", containerPrefix+"core", containerPrefix+"cabpk", containerPrefix+"kcp", containerPrefix+"capd")
}

// directManagerBinding describes one ClusterRoleBinding the driver creates
// because the shim pod runs no kube-controller-manager.
type directManagerBinding struct {
	// name is the binding name (namespaced by the capishim- prefix intent).
	name string
	// user is the manager client-cert CN the binding grants.
	user string
	// role is the concrete ClusterRole carrying the provider permissions.
	role string
}

// directManagerBindings are the bindings that replace ClusterRole
// aggregation. The vendored provider manifests bind the core and kcp manager
// identities to aggregated ClusterRoles (capi-aggregated-manager-role and
// capi-kubeadm-control-plane-aggregated-manager-role), which in stock
// clusters are populated by kube-controller-manager's clusterrole-aggregation
// controller. Since k8s 1.31 that controller lives in
// kube-controller-manager, and the shim pod has none (plan assumption 6: only
// etcd + apiserver + the four managers), so the aggregated roles would stay
// empty and the managers would have no permissions. The driver instead binds
// the manager identities directly to the concrete manager ClusterRoles the
// aggregation would have merged (the only roles carrying the aggregation
// labels in the v1.14 vendor), restoring the same effective permissions.
var directManagerBindings = []directManagerBinding{
	{name: "capishim-core-direct", user: "capishim:core-manager", role: "capi-manager-role"},
	{name: "capishim-kcp-direct", user: "capishim:kcp-manager", role: "capi-kubeadm-control-plane-manager-role"},
}

// ensureDirectManagerBindings creates (or updates) the direct manager
// ClusterRoleBindings against the management apiserver using the admin
// kubeconfig emitted by the setup container. It runs after setup, so the
// concrete ClusterRoles exist; it is idempotent so Restart can call it again.
func (p *ClusterProvider) ensureDirectManagerBindings(ctx context.Context) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", p.GetKubeconfigPath())
	if err != nil {
		return fmt.Errorf("build admin config for manager bindings: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build admin client for manager bindings: %w", err)
	}
	rbac := clientset.RbacV1().ClusterRoleBindings()
	for _, spec := range directManagerBindings {
		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: spec.name},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "ClusterRole",
				Name:     spec.role,
			},
			Subjects: []rbacv1.Subject{{
				APIGroup: rbacv1.GroupName,
				Kind:     rbacv1.UserKind,
				Name:     spec.user,
			}},
		}
		_, err := rbac.Create(ctx, binding, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			existing, getErr := rbac.Get(ctx, spec.name, metav1.GetOptions{})
			if getErr != nil {
				return fmt.Errorf("get ClusterRoleBinding %s: %w", spec.name, getErr)
			}
			binding.SetResourceVersion(existing.GetResourceVersion())
			_, err = rbac.Update(ctx, binding, metav1.UpdateOptions{})
		}
		if err != nil {
			return fmt.Errorf("ensure ClusterRoleBinding %s: %w", spec.name, err)
		}
	}
	return nil
}

// startOneshot starts a oneshot container (pki or setup) and waits for it to
// exit 0, surfacing its logs on failure for diagnosis.
func (p *ClusterProvider) startOneshot(ctx context.Context, id string) error {
	name := containerPrefix + id
	if _, err := runPodman(ctx, "start", name); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	out, err := runPodman(ctx, "wait", name)
	if err != nil {
		return fmt.Errorf("wait for %s: %w", name, err)
	}
	code, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return fmt.Errorf("parse %s exit code %q: %w", name, out, err)
	}
	if code != 0 {
		logs, _ := runPodman(ctx, "logs", name)
		return fmt.Errorf("%s exited %d: %s", name, code, strings.TrimSpace(logs))
	}
	return nil
}

// waitForHealthz polls the apiserver /healthz endpoint until it returns 200,
// using the pod CA and the admin client certificate (mirroring the setup
// container's own probe). The wait is bounded by healthzTimeout (VC-01).
func (p *ClusterProvider) waitForHealthz(ctx context.Context) error {
	caPEM, err := os.ReadFile(filepath.Join(p.stateDir, "pki", "ca.crt"))
	if err != nil {
		return fmt.Errorf("read pod CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("pod CA contains no parseable certificates")
	}
	clientCert, err := tls.LoadX509KeyPair(
		filepath.Join(p.stateDir, "pki", "admin.crt"),
		filepath.Join(p.stateDir, "pki", "admin.key"),
	)
	if err != nil {
		return fmt.Errorf("load healthz probe client certificate: %w", err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:      pool,
				Certificates: []tls.Certificate{clientCert},
				MinVersion:   tls.VersionTLS12,
			},
		},
		Timeout: healthzProbeTimeout,
	}
	url := "https://127.0.0.1:" + p.apiserverPort() + "/healthz"
	deadline := time.Now().Add(healthzTimeout)
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build healthz probe: %w", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			if time.Now().After(deadline) {
				return fmt.Errorf("apiserver not ready within %s: %w", healthzTimeout, lastErr)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("wait for apiserver: %w", ctx.Err())
			case <-time.After(healthzPollInterval):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("healthz returned %s", resp.Status)
			_ = resp.Body.Close()
			if time.Now().After(deadline) {
				return fmt.Errorf("apiserver not ready within %s: %w", healthzTimeout, lastErr)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("wait for apiserver: %w", ctx.Err())
			case <-time.After(healthzPollInterval):
			}
			continue
		}
		if err := resp.Body.Close(); err != nil {
			return fmt.Errorf("close healthz response: %w", err)
		}
		return nil
	}
}

// waitForRunning polls ContainerStates until every named container reports the
// running state, bounded by runningTimeout. A container that exits during the
// wait (for example a manager that fails to start) is surfaced as an error.
func (p *ClusterProvider) waitForRunning(ctx context.Context, names ...string) error {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	deadline := time.Now().Add(runningTimeout)
	for {
		states, err := p.ContainerStates(ctx)
		if err != nil {
			return fmt.Errorf("wait for running containers: %w", err)
		}
		seen := make(map[string]string, len(states))
		for _, s := range states {
			seen[s.Name] = s.State
		}
		missing := make([]string, 0, len(want))
		for name := range want {
			state, ok := seen[name]
			if !ok {
				missing = append(missing, name)
				continue
			}
			if state != "running" {
				logs, _ := runPodman(ctx, "logs", name)
				return fmt.Errorf("container %s is %s, want running: %s", name, state, strings.TrimSpace(logs))
			}
		}
		if len(missing) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("containers not running within %s: %v", runningTimeout, missing)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for running containers: %w", ctx.Err())
		case <-time.After(healthzPollInterval):
		}
	}
}
