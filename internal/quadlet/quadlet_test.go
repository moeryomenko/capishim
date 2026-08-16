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
//     section with Type=oneshot and RemainAfterExit=yes (plan assumption 3).
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
//     etcd mounts <state>/etcd rw, apiserver mounts <state>/pki ro, setup
//     mounts <state>/pki ro and <state>/kubeconfigs rw, each manager mounts
//     <state>/kubeconfigs ro and <state>/pki/<id>-webhook ro (REQ-009 layout).
//   - Manager units carry a Command= line supplying the /manager flags
//     (REQ-006; the provider image entrypoints are the stock binaries, so
//     quadlet Command= is the spec-faithful carrier): --kubeconfig=<state>
//     /kubeconfigs/<id>.kubeconfig, --webhook-port=<table port>,
//     --webhook-cert-dir=<state>/pki/<id>-webhook, --leader-elect, and
//     --leader-election-namespace=<table namespace>. Core additionally carries
//     --feature-gates=ClusterTopology=true; no non-core manager does.
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
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, key+"=") {
			vals = append(vals, strings.TrimPrefix(line, key+"="))
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

// wantUnitNames returns the nine unit filenames the renderer must produce.
func wantUnitNames() map[string]bool {
	want := map[string]bool{"capishim.pod": true}
	for _, spec := range config.Components() {
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
		name := "capishim-" + string(spec.ID) + ".container"
		if strings.Contains(units[name], "Type=oneshot") {
			t.Errorf("%s must not be a oneshot unit:\n%s", name, units[name])
		}
	}
}

func TestRenderImagesMatchComponentTable(t *testing.T) {
	t.Parallel()
	// Empty Version exercises the CAPISHIM_VERSION fallback: the rendered
	// Image= lines must equal the table exactly, which uses :v0.1.0.
	units := renderWith(t, "", testBind)
	for _, spec := range config.Components() {
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
		for _, v := range unitValues(t, units[name], "Volume") {
			if v == want {
				return
			}
		}
		t.Errorf("%s missing Volume=%s; got %v", name, want, unitValues(t, units[name], "Volume"))
	}

	wantIn("capishim-pki.container", mount("pki", false))
	wantIn("capishim-etcd.container", mount("etcd", false))
	wantIn("capishim-apiserver.container", mount("pki", true))
	wantIn("capishim-setup.container", mount("pki", true))
	wantIn("capishim-setup.container", mount("kubeconfigs", false))
	for _, spec := range config.Components() {
		if spec.WebhookPort == 0 {
			continue
		}
		name := "capishim-" + string(spec.ID) + ".container"
		wantIn(name, mount("kubeconfigs", true))
		wantIn(name, mount(filepath.Join("pki", string(spec.ID)+"-webhook"), true))
	}
}

func TestRenderManagerCommand(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	wantFlags := func(id config.ComponentID) []string {
		spec, ok := config.Component(id)
		if !ok {
			t.Fatalf("config.Component(%q) not found", id)
		}
		return []string{
			"--kubeconfig=" + filepath.Join(testStateDir, spec.Kubeconfig),
			"--webhook-port=" + strconv.Itoa(spec.WebhookPort),
			"--webhook-cert-dir=" + filepath.Join(testStateDir, "pki", string(spec.ID)+"-webhook"),
			"--leader-elect",
			"--leader-election-namespace=" + spec.ProviderNamespace,
		}
	}
	managerIDs := []config.ComponentID{config.ComponentCore, config.ComponentCABPK, config.ComponentKCP, config.ComponentCAPD}
	for _, id := range managerIDs {
		t.Run(string(id), func(t *testing.T) {
			t.Parallel()
			name := "capishim-" + string(id) + ".container"
			cmd := unitValue(t, units[name], "Command")
			for _, flag := range wantFlags(id) {
				if !strings.Contains(cmd, flag) {
					t.Errorf("%s Command missing %s; got %q", name, flag, cmd)
				}
			}
		})
	}
	// REQ-006: only core carries the ClusterTopology feature gate.
	core := unitValue(t, units["capishim-core.container"], "Command")
	if !strings.Contains(core, "--feature-gates=ClusterTopology=true") {
		t.Errorf("capishim-core.container Command missing --feature-gates=ClusterTopology=true; got %q", core)
	}
	for _, id := range []config.ComponentID{config.ComponentCABPK, config.ComponentKCP, config.ComponentCAPD} {
		name := "capishim-" + string(id) + ".container"
		if cmd := unitValue(t, units[name], "Command"); strings.Contains(cmd, "--feature-gates") {
			t.Errorf("%s Command carries --feature-gates, want core-only (REQ-006):\n%s", name, cmd)
		}
	}
}

func TestRenderEnvironment(t *testing.T) {
	t.Parallel()
	units := renderWith(t, testVersion, testBind)
	wantIn := func(name, want string) {
		t.Helper()
		for _, v := range unitValues(t, units[name], "Environment") {
			if v == want {
				return
			}
		}
		t.Errorf("%s missing Environment=%s; got %v", name, want, unitValues(t, units[name], "Environment"))
	}

	// The capishim-built containers consume configuration via config.Load(env):
	// pki needs the state dir, setup needs the state dir and the bind address
	// (REQ-010). Manager flags travel in Command= instead, asserted by
	// TestRenderManagerCommand.
	wantIn("capishim-pki.container", config.EnvStateDir+"="+testStateDir)
	wantIn("capishim-setup.container", config.EnvStateDir+"="+testStateDir)
	wantIn("capishim-setup.container", config.EnvBindAddress+"="+testBind)
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
