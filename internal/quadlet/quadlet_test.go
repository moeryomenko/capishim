// Package quadlet_test contains the red-phase tests for the internal/quadlet
// API designed in TASK-013 (test-first). These tests lock the contract that
// TASK-014 must implement: the renderer that produces the quadlet pod unit and
// the eight container units from the config component table.
//
// The locked contract:
//
//   - API: quadlet.Input{Config: config.Config, Version: string}, and the two
//     entry points quadlet.Render(Input) (map[string]string, error) keyed by
//     unit filename and quadlet.RenderUnit(name string, Input) (string, error)
//     for a single unit. Render returns exactly nine units: capishim.pod plus
//     capishim-<id>.container for each of the eight components in the config
//     table (REQ-001 as amended by plan assumption 2: a dedicated pki
//     container is the first element of the boot chain).
//   - Ordering: every link of the chain pki -> etcd -> apiserver -> setup ->
//     managers is encoded with both Requires= and After= referencing the
//     systemd unit name (capishim-<id>.service), so a failure in pki stops the
//     stack instead of letting dependents start without certs (REQ-001 names
//     "Requires=/After=" as the mechanism). pki is the root and orders after
//     no sibling.
//   - pki and setup are oneshot containers: their units carry a [Service]
//     section with Type=oneshot and RemainAfterExit=yes (plan assumption 3)
//     and an Exec= line dispatching the entrypoint subcommand — "pki" and
//     "setup" respectively (podman quadlet appends Exec= to the image
//     ENTRYPOINT ["/capishim"], so the bare subcommand reaches main.go's
//     dispatch; amended after TASK-021: a oneshot unit without Exec= starts
//     the entrypoint with no subcommand, which exits 0 doing nothing and
//     breaks VC-01 via the systemd path).
//   - The pod publishes the apiserver: PublishPort=<bind address>:6443, i.e.
//     CAPISHIM_BIND_ADDRESS overrides the host part and the container port
//     stays fixed at 6443 (REQ-010).
//   - Image= lines come from the config component table. For refs with the
//     localhost/capishim-* prefix the tag is rewritten to Input.Version; an
//     empty Version falls back to v0.1.0 (plan assumption 7). Stock refs
//     (registry.k8s.io/...) are emitted verbatim. The rewrite derives from the
//     table image, not the component ID: pki reuses the capishim-setup image.
//   - Volume= lines mount the state directory at the same path inside the
//     container with explicit :rw/:ro semantics: pki mounts <state>/pki rw,
//     etcd mounts <state>/etcd rw and <state>/pki ro (etcd reads its TLS certs
//     from pki), apiserver mounts <state>/pki ro and <state>/abac ro, setup
//     mounts <state>/pki ro and <state>/kubeconfigs rw, each manager mounts
//     <state>/pki ro, <state>/kubeconfigs ro and <state>/pki/<id>-webhook ro
//     (REQ-009 layout corrected by the e2e proof).
//   - Manager units carry an Exec= line supplying the /manager flags (the
//     proven runtime contract from the e2e podman driver,
//     e2e/shim/components.go; REQ-006 as corrected): --kubeconfig=<state>
//     /kubeconfigs/<id>.kubeconfig, --webhook-port=<table port>,
//     --webhook-cert-dir=<state>/pki/<id>-webhook,
//     --health-addr=127.0.0.1:<health port> and
//     --diagnostics-address=127.0.0.1:<diagnostics port> (per-manager ports
//     core 9451/8451, cabpk 9452/8452, kcp 9453/8453, capd 9454/8454 — literal
//     here until a TASK-002/003 amendment adds HealthPort/DiagnosticsPort to
//     ComponentSpec), and --feature-gates=ClusterTopology=true on ALL FOUR
//     managers (bootstrap and infrastructure webhooks gate topology fields; the
//     REQ-006 "core only" letter is superseded by the e2e proof). No
//     --leader-elect or --leader-election-namespace anywhere: the v1.14
//     binaries reject the namespace flag and controller-runtime cannot default
//     an election namespace outside a cluster (plan assumption 5 superseded).
//   - The etcd unit carries an Exec= line with the full working flag set
//     mirroring the e2e driver: client and peer TLS on loopback with certs from
//     <state>/pki (plan assumption 8 is superseded: peer traffic is TLS too).
//   - The apiserver unit carries an Exec= line with the full working flag set:
//     --etcd-servers + etcd TLS client certs, apiserver serving cert, SA
//     signing keypair, --authorization-mode=ABAC,RBAC with
//     --authorization-policy-file=<state>/abac/policy.json (kube-apiserver
//     v1.36 removed --authorization-rbac-super-user, so the setup container
//     must bootstrap RBAC from a clean cluster), --bind-address=0.0.0.0
//     (rootless podman forwards host traffic to the pod IP, not pod loopback),
//     --secure-port=6443 and the cluster CIDR.
//   - pki, setup and the four managers run as User=0: the distroless nonroot
//     uid (65532) cannot write a host-owned state dir under rootless podman.
//   - The capd unit carries Environment=POD_IP=127.0.0.1: the in-memory backend
//     mux host becomes the workload ControlPlaneEndpoint and the server of the
//     generated <cluster>-kubeconfig Secrets (REQ-008).
//   - Environment= lines carry only the values the capishim-built binaries
//     consume via config.Load (REQ-010): CAPISHIM_STATE_DIR on pki and setup,
//     and CAPISHIM_BIND_ADDRESS on setup.
//   - Rendering is deterministic and rejects invalid input: empty StateDir or
//     empty BindAddress is an error (mirroring config.Load's validation), and
//     RenderUnit rejects unknown unit names.
//
// Until TASK-014 lands, the package has no non-test Go files and `go test
// ./internal/quadlet` fails to compile. That failure is the red phase.
package quadlet_test

import (
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/moeryomenko/capishim/internal/config"
	"github.com/moeryomenko/capishim/internal/quadlet"
)

const (
	testStateDir = "/srv/capishim"
	testBind     = "127.0.0.1:6443"
	testVersion  = "v0.1.0"
)

// renderWith renders the full unit set for the fixed state dir and the given
// version and bind address. A fatal failure here means the renderer rejected
// input the tests consider valid.
func renderWith(t *testing.T, version, bind string) map[string]string {
	t.Helper()
	units, err := quadlet.Render(quadlet.Input{
		Config:  config.Config{StateDir: testStateDir, BindAddress: bind},
		Version: version,
	})
	if err != nil {
		t.Fatalf("Render(StateDir=%q, BindAddress=%q, Version=%q) returned error: %v", testStateDir, bind, version, err)
	}
	return units
}

// unitValue returns the value of the single `key=value` line in unit.
func unitValue(t *testing.T, unit, key string) string {
	t.Helper()
	vals := unitValues(t, unit, key)
	if len(vals) > 1 {
		t.Fatalf("unit has %d %s= lines, want exactly one: %v", len(vals), key, vals)
	}
	return vals[0]
}

// unitValues returns every `key=value` value in unit (for multi-valued keys
// like Volume= and Environment=).
func unitValues(t *testing.T, unit, key string) []string {
	t.Helper()
	var vals []string
	for line := range strings.SplitSeq(unit, "\n") {
		if after, ok := strings.CutPrefix(line, key+"="); ok {
			vals = append(vals, after)
		}
	}
	if len(vals) == 0 {
		t.Fatalf("unit has no %s= lines\n---\n%s\n---", key, unit)
	}
	return vals
}

// unitContains fails unless unit contains want as a raw substring.
func unitContains(t *testing.T, unit, want string) {
	t.Helper()
	if !strings.Contains(unit, want) {
		t.Errorf("unit does not contain %q\n---\n%s\n---", want, unit)
	}
}

// sectionOf returns the name of the unit section that contains the first
// `key=` line, or "" when the key appears before any section header. It fails
// the test when the key is absent.
func sectionOf(t *testing.T, unit, key string) string {
	t.Helper()
	section := ""
	for line := range strings.SplitSeq(unit, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		if strings.HasPrefix(line, key+"=") {
			return section
		}
	}
	t.Fatalf("unit has no %s= lines\n---\n%s\n---", key, unit)
	return ""
}

// wantUnitNames returns the nine unit filenames the renderer must produce:
// the pod plus one container per in-pod component (external specs have no
// container unit, REQ-004).
func wantUnitNames() map[string]bool {
	want := map[string]bool{"capishim.pod": true}
	for _, spec := range config.Components() {
		if spec.External {
			continue
		}
		want["capishim-"+string(spec.ID)+".container"] = true
	}
	return want
}

// localImageWithVersion rewrites the tag of a localhost/capishim-* image ref.
func localImageWithVersion(ref, version string) string {
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return ref
	}
	return ref[:i] + ":" + version
}

func TestRenderUnitSet(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	want := wantUnitNames()
	for name := range want {
		if _, ok := units[name]; !ok {
			t.Errorf("Render output missing unit %q", name)
		}
	}
	for name := range units {
		if !want[name] {
			t.Errorf("Render output has unexpected unit %q", name)
		}
	}
	if got := len(units); got != 9 {
		t.Errorf("Render returned %d units, want 9", got)
	}
}

func TestRenderPodWiring(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	for _, spec := range config.Components() {
		if spec.External {
			continue
		}
		name := "capishim-" + string(spec.ID) + ".container"
		unitContains(t, units[name], "[Container]")
		if got := unitValue(t, units[name], "Pod"); got != "capishim.pod" {
			t.Errorf("%s Pod = %q, want %q", name, got, "capishim.pod")
		}
	}
}

func TestRenderPodPublishPort(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	pod := units["capishim.pod"]
	unitContains(t, pod, "[Pod]")
	if got := unitValue(t, pod, "PublishPort"); got != "127.0.0.1:6443:6443" {
		t.Errorf("PublishPort = %q, want %q", got, "127.0.0.1:6443:6443")
	}
}

func TestRenderPodPublishPortCustomBind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		bind string
		want string
	}{
		{"wildcard IPv4", "0.0.0.0:6443", "0.0.0.0:6443:6443"},
		{"loopback IPv6", "[::1]:6443", "[::1]:6443:6443"},
		{"wildcard IPv6", "[::]:6443", "[::]:6443:6443"},
		{"custom host port keeps container port", "127.0.0.1:8443", "127.0.0.1:8443:6443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			units := renderWith(t, testVersion, tt.bind)
			if got := unitValue(t, units["capishim.pod"], "PublishPort"); got != tt.want {
				t.Errorf("PublishPort for bind %q = %q, want %q", tt.bind, got, tt.want)
			}
			// TASK-023: the pod must keep running when the pki oneshot exits,
			// so ExitPolicy=continue is required for every bind variant too.
			if got := unitValue(t, units["capishim.pod"], "ExitPolicy"); got != "continue" {
				t.Errorf("ExitPolicy for bind %q = %q, want %q", tt.bind, got, "continue")
			}
		})
	}
}

func TestRenderPodExitPolicyContinue(t *testing.T) {
	t.Parallel()
	// TASK-023 defect: podman quadlet defaults pod units to ExitPolicy=stop,
	// so the pki oneshot container — the only container running while etcd
	// waits on it — exiting ~90ms after start (certs already exist) tears the
	// whole pod down and VC-01 fails via the systemd path: pod inactive
	// (dead), admin.kubeconfig never created. The rendered pod unit must pin
	// ExitPolicy=continue so oneshot exits do not stop the pod.
	tests := []struct {
		name string
		bind string
	}{
		{"loopback IPv4", "127.0.0.1:6443"},
		{"wildcard IPv4", "0.0.0.0:6443"},
		{"loopback IPv6", "[::1]:6443"},
		{"wildcard IPv6", "[::]:6443"},
		{"custom host port", "127.0.0.1:8443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			units := renderWith(t, testVersion, tt.bind)
			pod := units["capishim.pod"]
			unitContains(t, pod, "[Pod]")
			// The directive must live in the [Pod] section, not a [Service]
			// section: podman reads ExitPolicy from [Pod] only.
			if got := sectionOf(t, pod, "ExitPolicy"); got != "[Pod]" {
				t.Errorf("ExitPolicy= line is in section %q, want [Pod]\n---\n%s\n---", got, pod)
			}
			if got := unitValue(t, pod, "ExitPolicy"); got != "continue" {
				t.Errorf("ExitPolicy = %q, want %q", got, "continue")
			}
			// Existing behavior preserved: the pod still publishes the
			// apiserver port for this bind address.
			if got := unitValue(t, pod, "PublishPort"); got != tt.bind+":6443" {
				t.Errorf("PublishPort for bind %q = %q, want %q", tt.bind, got, tt.bind+":6443")
			}
		})
	}
}

func TestRenderOrderingChain(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	links := []struct {
		unit string
		dep  string
	}{
		{"capishim-etcd.container", "capishim-pki.service"},
		{"capishim-apiserver.container", "capishim-etcd.service"},
		{"capishim-setup.container", "capishim-apiserver.service"},
		{"capishim-core.container", "capishim-setup.service"},
		{"capishim-cabpk.container", "capishim-setup.service"},
		{"capishim-kcp.container", "capishim-setup.service"},
		{"capishim-capd.container", "capishim-setup.service"},
	}
	for _, link := range links {
		if got := unitValue(t, units[link.unit], "Requires"); got != link.dep {
			t.Errorf("%s Requires = %q, want %q", link.unit, got, link.dep)
		}
		if got := unitValue(t, units[link.unit], "After"); got != link.dep {
			t.Errorf("%s After = %q, want %q", link.unit, got, link.dep)
		}
	}
	// pki is the root of the chain: it must not order after or require any
	// sibling unit.
	pki := units["capishim-pki.container"]
	if strings.Contains(pki, "Requires=capishim-") || strings.Contains(pki, "After=capishim-") {
		t.Errorf("capishim-pki.container must not require or order after sibling units:\n%s", pki)
	}
}

func TestRenderOneshotUnits(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	for _, name := range []string{"capishim-pki.container", "capishim-setup.container"} {
		unitContains(t, units[name], "[Service]")
		if got := unitValue(t, units[name], "Type"); got != "oneshot" {
			t.Errorf("%s Type = %q, want %q", name, got, "oneshot")
		}
		if got := unitValue(t, units[name], "RemainAfterExit"); got != "yes" {
			t.Errorf("%s RemainAfterExit = %q, want %q", name, got, "yes")
		}
	}
	for _, spec := range config.Components() {
		if spec.ID == config.ComponentPKI || spec.ID == config.ComponentSetup {
			continue
		}
		if spec.External {
			continue
		}
		name := "capishim-" + string(spec.ID) + ".container"
		if strings.Contains(units[name], "Type=oneshot") {
			t.Errorf("%s must not be a oneshot unit:\n%s", name, units[name])
		}
	}
}

func TestRenderOneshotExec(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	// TASK-021 defect: the rendered pki and setup units carried no Exec= line,
	// so `systemctl --user start capishim-pod` ran them as no-ops — the
	// entrypoint exits 0 without a subcommand (cmd/capishim/main.go) — and
	// VC-01 failed via the systemd path. The oneshot units must dispatch
	// their subcommand through Exec= exactly as the proven e2e driver does
	// (e2e/shim/components.go passes "pki" and "setup" explicitly).
	//
	// TASK-025 defect (TASK-025 red phase): podman quadlet appends Exec= to the
	// image ENTRYPOINT rather than replacing it, so the Exec line must carry
	// the subcommand alone. The capishim-setup image sets
	// ENTRYPOINT ["/capishim"] (images/capishim-setup.Containerfile), so
	// Exec=/capishim pki would run `/capishim /capishim pki`: the first
	// argument "/capishim" matches no subcommand in cmd/capishim/main.go's
	// switch, falls through to flag parsing, and exits 0 doing nothing. The
	// rendered Exec must be exactly "pki" / "setup" (no leading slash, no
	// binary path) so the entrypoint actually dispatches — the same mechanism
	// the manager units rely on when they pass only flags and the /manager
	// ENTRYPOINT supplies the binary.
	tests := []struct {
		name string
		want string
	}{
		{name: "capishim-pki.container", want: "pki"},
		{name: "capishim-setup.container", want: "setup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			exec := unitValue(t, units[tt.name], "Exec")
			if exec != tt.want {
				t.Errorf("%s Exec = %q, want %q", tt.name, exec, tt.want)
			}
		})
	}
}

func TestRenderImagesMatchComponentTable(t *testing.T) {
	t.Parallel()
	// Empty Version exercises the CAPISHIM_VERSION fallback: the rendered
	// Image= lines must equal the table exactly, which uses :v0.1.0.
	units := renderWith(t, "", testBind)
	for _, spec := range config.Components() {
		if spec.External {
			continue
		}
		name := "capishim-" + string(spec.ID) + ".container"
		if got := unitValue(t, units[name], "Image"); got != spec.Image {
			t.Errorf("%s Image = %q, want table value %q", name, got, spec.Image)
		}
	}
}

func TestRenderImageVersionSubstitution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		version string
	}{
		{"versioned tag", "v1.2.3"},
		{"version used verbatim without v prefix", "0.2.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			units := renderWith(t, tt.version, testBind)
			for _, spec := range config.Components() {
				if spec.External {
					continue
				}
				name := "capishim-" + string(spec.ID) + ".container"
				got := unitValue(t, units[name], "Image")
				want := spec.Image
				if strings.HasPrefix(spec.Image, "localhost/capishim-") {
					// Tag rewrite applies to local refs only; stock refs
					// (registry.k8s.io/...) stay verbatim. The rewrite follows
					// the table image so pki reuses the capishim-setup image.
					want = localImageWithVersion(spec.Image, tt.version)
				}
				if got != want {
					t.Errorf("%s Image with Version %q = %q, want %q", name, tt.version, got, want)
				}
			}
		})
	}
}

func TestRenderVolumes(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	mount := func(sub string, ro bool) string {
		src := filepath.Join(testStateDir, sub)
		opts := "rw"
		if ro {
			opts = "ro"
		}
		return src + ":" + src + ":" + opts
	}
	wantIn := func(name, want string) {
		t.Helper()
		if slices.Contains(unitValues(t, units[name], "Volume"), want) {
			return
		}
		t.Errorf("%s missing Volume=%s; got %v", name, want, unitValues(t, units[name], "Volume"))
	}

	wantIn("capishim-pki.container", mount("pki", false))
	wantIn("capishim-pki.container", mount("webhook-certs", false))
	wantIn("capishim-etcd.container", mount("etcd", false))
	wantIn("capishim-etcd.container", mount("pki", true))
	wantIn("capishim-apiserver.container", mount("pki", true))
	wantIn("capishim-apiserver.container", mount("abac", true))
	wantIn("capishim-setup.container", mount("pki", true))
	wantIn("capishim-setup.container", mount("webhook-certs", false))
	wantIn("capishim-setup.container", mount("kubeconfigs", false))
	for _, spec := range config.Components() {
		if spec.WebhookPort == 0 || spec.External {
			continue
		}
		name := "capishim-" + string(spec.ID) + ".container"
		wantIn(name, mount("pki", true))
		wantIn(name, mount("kubeconfigs", true))
		wantIn(name, mount(filepath.Join("pki", string(spec.ID)+"-webhook"), true))
	}
}

func TestRenderManagerExec(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	// Health and diagnostics ports are not yet fields of config.ComponentSpec
	// (a TASK-002/003 amendment is needed); the proven e2e driver assigns the
	// per-manager values below because the manager defaults collide inside the
	// shared pod network namespace.
	healthPorts := map[config.ComponentID]int{
		config.ComponentCore:  9451,
		config.ComponentCABPK: 9452,
		config.ComponentKCP:   9453,
		config.ComponentCAPD:  9454,
	}
	diagnosticsPorts := map[config.ComponentID]int{
		config.ComponentCore:  8451,
		config.ComponentCABPK: 8452,
		config.ComponentKCP:   8453,
		config.ComponentCAPD:  8454,
	}
	wantFlags := func(id config.ComponentID) []string {
		spec, ok := config.Component(id)
		if !ok {
			t.Fatalf("config.Component(%q) not found", id)
		}
		return []string{
			"--kubeconfig=" + filepath.Join(testStateDir, spec.Kubeconfig),
			"--webhook-port=" + strconv.Itoa(spec.WebhookPort),
			"--webhook-cert-dir=" + filepath.Join(testStateDir, "pki", string(spec.ID)+"-webhook"),
			"--health-addr=127.0.0.1:" + strconv.Itoa(healthPorts[id]),
			"--diagnostics-address=127.0.0.1:" + strconv.Itoa(diagnosticsPorts[id]),
			// REQ-006 corrected by the e2e proof: every manager webhook gates
			// topology fields, so all four run with ClusterTopology=true.
			"--feature-gates=ClusterTopology=true",
		}
	}
	managerIDs := []config.ComponentID{
		config.ComponentCore,
		config.ComponentCABPK,
		config.ComponentKCP,
		config.ComponentCAPD,
	}
	for _, id := range managerIDs {
		t.Run(string(id), func(t *testing.T) {
			t.Parallel()
			name := "capishim-" + string(id) + ".container"
			exec := unitValue(t, units[name], "Exec")
			for _, flag := range wantFlags(id) {
				if !strings.Contains(exec, flag) {
					t.Errorf("%s Exec missing %s; got %q", name, flag, exec)
				}
			}
			// The v1.14 binaries reject --leader-election-namespace and
			// controller-runtime cannot default an election namespace outside a
			// cluster: no leader-election flags anywhere (plan assumption 5
			// superseded by the e2e proof).
			if strings.Contains(exec, "--leader-elect") || strings.Contains(exec, "--leader-election-namespace") {
				t.Errorf("%s Exec carries leader-election flags (superseded):\n%s", name, exec)
			}
		})
	}
}

func TestRenderEtcdExec(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	exec := unitValue(t, units["capishim-etcd.container"], "Exec")
	wantFlags := []string{
		"etcd --name=capishim-etcd",
		"--data-dir=" + filepath.Join(testStateDir, "etcd"),
		"--listen-client-urls=https://127.0.0.1:2379",
		"--advertise-client-urls=https://127.0.0.1:2379",
		"--listen-peer-urls=https://127.0.0.1:2380",
		"--initial-advertise-peer-urls=https://127.0.0.1:2380",
		"--initial-cluster=capishim-etcd=https://127.0.0.1:2380",
		"--cert-file=" + filepath.Join(testStateDir, "pki", "etcd-server.crt"),
		"--key-file=" + filepath.Join(testStateDir, "pki", "etcd-server.key"),
		"--trusted-ca-file=" + filepath.Join(testStateDir, "pki", "ca.crt"),
		"--client-cert-auth=true",
		"--peer-cert-file=" + filepath.Join(testStateDir, "pki", "etcd-server.crt"),
		"--peer-key-file=" + filepath.Join(testStateDir, "pki", "etcd-server.key"),
		"--peer-trusted-ca-file=" + filepath.Join(testStateDir, "pki", "ca.crt"),
		"--peer-client-cert-auth=true",
	}
	for _, flag := range wantFlags {
		if !strings.Contains(exec, flag) {
			t.Errorf("capishim-etcd.container Exec missing %s; got %q", flag, exec)
		}
	}
}

func TestRenderAPIServerExec(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	exec := unitValue(t, units["capishim-apiserver.container"], "Exec")
	wantFlags := []string{
		"kube-apiserver --etcd-servers=https://127.0.0.1:2379",
		"--etcd-cafile=" + filepath.Join(testStateDir, "pki", "ca.crt"),
		"--etcd-certfile=" + filepath.Join(testStateDir, "pki", "apiserver-client.crt"),
		"--etcd-keyfile=" + filepath.Join(testStateDir, "pki", "apiserver-client.key"),
		"--client-ca-file=" + filepath.Join(testStateDir, "pki", "ca.crt"),
		"--tls-cert-file=" + filepath.Join(testStateDir, "pki", "apiserver.crt"),
		"--tls-private-key-file=" + filepath.Join(testStateDir, "pki", "apiserver.key"),
		"--service-account-key-file=" + filepath.Join(testStateDir, "pki", "sa.pub"),
		"--service-account-signing-key-file=" + filepath.Join(testStateDir, "pki", "sa.key"),
		"--service-account-issuer=https://127.0.0.1:6443",
		"--authorization-mode=ABAC,RBAC",
		"--authorization-policy-file=" + filepath.Join(testStateDir, "abac", "policy.json"),
		"--bind-address=0.0.0.0",
		"--secure-port=6443",
		"--service-cluster-ip-range=10.128.0.0/12",
		"--allow-privileged=true",
	}
	for _, flag := range wantFlags {
		if !strings.Contains(exec, flag) {
			t.Errorf("capishim-apiserver.container Exec missing %s; got %q", flag, exec)
		}
	}
}

func TestRenderUser(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	// The capishim-built images (pki, setup, managers) are distroless and run
	// as uid 65532 by default, which cannot write a host-owned state dir under
	// rootless podman; the proven driver forces User=0.
	for _, id := range []config.ComponentID{
		config.ComponentPKI,
		config.ComponentSetup,
		config.ComponentCore,
		config.ComponentCABPK,
		config.ComponentKCP,
		config.ComponentCAPD,
	} {
		name := "capishim-" + string(id) + ".container"
		if got := unitValue(t, units[name], "User"); got != "0" {
			t.Errorf("%s User = %q, want %q (distroless nonroot cannot write the host state dir)", name, got, "0")
		}
	}
}

func TestRenderEnvironment(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	wantIn := func(name, want string) {
		t.Helper()
		if slices.Contains(unitValues(t, units[name], "Environment"), want) {
			return
		}
		t.Errorf("%s missing Environment=%s; got %v", name, want, unitValues(t, units[name], "Environment"))
	}

	// The capishim-built containers consume configuration via config.Load(env):
	// pki needs the state dir, setup needs the state dir and the bind address
	// (REQ-010). Manager flags travel in Exec= instead, asserted by
	// TestRenderManagerExec.
	wantIn("capishim-pki.container", config.EnvStateDir+"="+testStateDir)
	wantIn("capishim-setup.container", config.EnvStateDir+"="+testStateDir)
	wantIn("capishim-setup.container", config.EnvBindAddress+"="+testBind)
	// The CAPD in-memory backend mux host comes from POD_IP and becomes the
	// workload ControlPlaneEndpoint host and <cluster>-kubeconfig server
	// (REQ-008). All managers share the pod network namespace, so loopback is
	// reachable.
	wantIn("capishim-capd.container", "POD_IP=127.0.0.1")
}

func TestRenderDeterministic(t *testing.T) {
	t.Parallel()
	first := renderWith(t, testVersion, testBind)
	second := renderWith(t, testVersion, testBind)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two renders of the same input differ:\nfirst:  %v\nsecond: %v", first, second)
	}
}

func TestRenderUnitSingle(t *testing.T) {
	t.Parallel()
	all := renderWith(t, testVersion, testBind)
	in := quadlet.Input{Config: config.Config{StateDir: testStateDir, BindAddress: testBind}, Version: testVersion}
	for name, want := range all {
		got, err := quadlet.RenderUnit(name, in)
		if err != nil {
			t.Fatalf("RenderUnit(%q) returned error: %v", name, err)
		}
		if got != want {
			t.Errorf("RenderUnit(%q) = %q, want the unit from Render = %q", name, got, want)
		}
	}
}

func TestRenderUnitUnknown(t *testing.T) {
	t.Parallel()
	in := quadlet.Input{Config: config.Config{StateDir: testStateDir, BindAddress: testBind}, Version: testVersion}
	for _, name := range []string{
		"capishim-nope.container",
		"capishim-pki.pod",
		"capishim.pod.container",
		"pki.container",
		"",
	} {
		if _, err := quadlet.RenderUnit(name, in); err == nil {
			t.Errorf("RenderUnit(%q) returned no error, want error for unknown unit", name)
		}
	}
}

func TestRenderInvalidConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  config.Config
	}{
		{name: "empty state dir", cfg: config.Config{StateDir: "", BindAddress: testBind}},
		{name: "empty bind address", cfg: config.Config{StateDir: testStateDir, BindAddress: ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := quadlet.Render(quadlet.Input{Config: tt.cfg, Version: testVersion}); err == nil {
				t.Errorf("Render with %s returned no error, want error", tt.name)
			}
			if _, err := quadlet.RenderUnit("capishim.pod", quadlet.Input{Config: tt.cfg, Version: testVersion}); err == nil {
				t.Errorf("RenderUnit with %s returned no error, want error", tt.name)
			}
		})
	}
}
