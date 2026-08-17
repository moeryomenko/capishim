// Component-table tests (TASK-002 red phase). The ComponentSpec table is the
// shared contract for TASK-014 (internal/quadlet rendering) and TASK-018 (e2e
// podman driver): they consume these exact IDs, images, ports, namespaces,
// prefixes, manager CNs and kubeconfig paths. Any value change here is a
// breaking change for those consumers, so the table is locked by tests.
package config_test

import (
	"path/filepath"
	"testing"

	"github.com/moeryomenko/capishim/internal/config"
)

func TestComponentsOrder(t *testing.T) {
	t.Parallel()
	want := []config.ComponentID{
		config.ComponentPKI,
		config.ComponentEtcd,
		config.ComponentAPIServer,
		config.ComponentSetup,
		config.ComponentCore,
		config.ComponentCABPK,
		config.ComponentKCP,
		config.ComponentCAPD,
	}
	got := config.Components()
	if len(got) != len(want) {
		t.Fatalf("Components() returned %d entries, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("Components()[%d].ID = %q, want %q (boot order pki -> etcd -> apiserver -> setup -> managers is locked)", i, got[i].ID, id)
		}
	}
}

func TestComponentsUniqueIDs(t *testing.T) {
	t.Parallel()
	seen := make(map[config.ComponentID]bool)
	for _, spec := range config.Components() {
		if seen[spec.ID] {
			t.Errorf("duplicate component ID %q in Components()", spec.ID)
		}
		seen[spec.ID] = true
	}
}

func TestComponentSpecTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id                  config.ComponentID
		wantImage           string
		wantWebhookPort     int
		wantHealthPort      int
		wantDiagnosticsPort int
		wantNamespace       string
		wantNamePrefix      string
		wantManagerCN       string
		wantKubeconfigRel   string
	}{
		{id: config.ComponentPKI, wantImage: "localhost/capishim-setup:v0.1.0"},
		{id: config.ComponentEtcd, wantImage: "registry.k8s.io/etcd:3.5.17-0"},
		{id: config.ComponentAPIServer, wantImage: "registry.k8s.io/kube-apiserver:v1.36.1"},
		{id: config.ComponentSetup, wantImage: "localhost/capishim-setup:v0.1.0"},
		{
			id:                  config.ComponentCore,
			wantImage:           "localhost/capishim-core:v0.1.0",
			wantWebhookPort:     9443,
			wantHealthPort:      9451,
			wantDiagnosticsPort: 8451,
			wantNamespace:       "capi-system",
			wantNamePrefix:      "capi-",
			wantManagerCN:       "capishim:core-manager",
			wantKubeconfigRel:   "kubeconfigs/core.kubeconfig",
		},
		{
			id:                  config.ComponentCABPK,
			wantImage:           "localhost/capishim-cabpk:v0.1.0",
			wantWebhookPort:     9444,
			wantHealthPort:      9452,
			wantDiagnosticsPort: 8452,
			wantNamespace:       "capi-kubeadm-bootstrap-system",
			wantNamePrefix:      "capi-kubeadm-bootstrap-",
			wantManagerCN:       "capishim:cabpk-manager",
			wantKubeconfigRel:   "kubeconfigs/cabpk.kubeconfig",
		},
		{
			id:                  config.ComponentKCP,
			wantImage:           "localhost/capishim-kcp:v0.1.0",
			wantWebhookPort:     9445,
			wantHealthPort:      9453,
			wantDiagnosticsPort: 8453,
			wantNamespace:       "capi-kubeadm-control-plane-system",
			wantNamePrefix:      "capi-kubeadm-control-plane-",
			wantManagerCN:       "capishim:kcp-manager",
			wantKubeconfigRel:   "kubeconfigs/kcp.kubeconfig",
		},
		{
			id:                  config.ComponentCAPD,
			wantImage:           "localhost/capishim-capd:v0.1.0",
			wantWebhookPort:     9446,
			wantHealthPort:      9454,
			wantDiagnosticsPort: 8454,
			wantNamespace:       "capd-system",
			wantNamePrefix:      "capd-",
			wantManagerCN:       "capishim:capd-manager",
			wantKubeconfigRel:   "kubeconfigs/capd.kubeconfig",
		},
	}
	for _, tt := range tests {
		t.Run(string(tt.id), func(t *testing.T) {
			t.Parallel()
			spec, ok := config.Component(tt.id)
			if !ok {
				t.Fatalf("Component(%q) not found", tt.id)
			}
			if spec.ID != tt.id {
				t.Errorf("spec.ID = %q, want %q", spec.ID, tt.id)
			}
			if spec.Image != tt.wantImage {
				t.Errorf("spec.Image = %q, want %q", spec.Image, tt.wantImage)
			}
			if spec.WebhookPort != tt.wantWebhookPort {
				t.Errorf("spec.WebhookPort = %d, want %d", spec.WebhookPort, tt.wantWebhookPort)
			}
			if spec.HealthPort != tt.wantHealthPort {
				t.Errorf("spec.HealthPort = %d, want %d", spec.HealthPort, tt.wantHealthPort)
			}
			if spec.DiagnosticsPort != tt.wantDiagnosticsPort {
				t.Errorf("spec.DiagnosticsPort = %d, want %d", spec.DiagnosticsPort, tt.wantDiagnosticsPort)
			}
			if spec.ProviderNamespace != tt.wantNamespace {
				t.Errorf("spec.ProviderNamespace = %q, want %q", spec.ProviderNamespace, tt.wantNamespace)
			}
			if spec.NamePrefix != tt.wantNamePrefix {
				t.Errorf("spec.NamePrefix = %q, want %q", spec.NamePrefix, tt.wantNamePrefix)
			}
			if spec.ManagerCN != tt.wantManagerCN {
				t.Errorf("spec.ManagerCN = %q, want %q", spec.ManagerCN, tt.wantManagerCN)
			}
			if spec.Kubeconfig != tt.wantKubeconfigRel {
				t.Errorf("spec.Kubeconfig = %q, want %q", spec.Kubeconfig, tt.wantKubeconfigRel)
			}
		})
	}
}

func TestComponentUnknownLookup(t *testing.T) {
	t.Parallel()
	unknown := []config.ComponentID{"does-not-exist", "", "CORE", "coree"}
	for _, id := range unknown {
		if spec, ok := config.Component(id); ok {
			t.Errorf("Component(%q) returned ok=true with spec %+v, want ok=false", id, spec)
		}
	}
}

func TestComponentsCopySemantics(t *testing.T) {
	t.Parallel()
	first := config.Components()
	if len(first) == 0 {
		t.Fatal("Components() returned an empty slice")
	}
	orig := first[0].Image
	first[0].Image = "tampered"
	second := config.Components()
	if second[0].Image != orig {
		t.Errorf("mutating a returned Components() leaked into the next call: Image = %q, want %q", second[0].Image, orig)
	}
}

func TestConfigKubeconfigPath(t *testing.T) {
	t.Parallel()
	cfg := config.Config{StateDir: "/srv/capishim"}
	tests := []struct {
		id   config.ComponentID
		want string
		ok   bool
	}{
		{id: config.ComponentCore, want: filepath.Join("/srv/capishim", "kubeconfigs", "core.kubeconfig"), ok: true},
		{id: config.ComponentCABPK, want: filepath.Join("/srv/capishim", "kubeconfigs", "cabpk.kubeconfig"), ok: true},
		{id: config.ComponentKCP, want: filepath.Join("/srv/capishim", "kubeconfigs", "kcp.kubeconfig"), ok: true},
		{id: config.ComponentCAPD, want: filepath.Join("/srv/capishim", "kubeconfigs", "capd.kubeconfig"), ok: true},
		{id: config.ComponentPKI, ok: false},
		{id: config.ComponentEtcd, ok: false},
		{id: config.ComponentAPIServer, ok: false},
		{id: config.ComponentSetup, ok: false},
		{id: "unknown", ok: false},
	}
	for _, tt := range tests {
		t.Run(string(tt.id), func(t *testing.T) {
			t.Parallel()
			got, ok := cfg.KubeconfigPath(tt.id)
			if ok != tt.ok {
				t.Fatalf("KubeconfigPath(%q) ok = %v, want %v", tt.id, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("KubeconfigPath(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestConfigAdminKubeconfigPath(t *testing.T) {
	t.Parallel()
	cfg := config.Config{StateDir: "/srv/capishim"}
	want := filepath.Join("/srv/capishim", "kubeconfigs", "admin.kubeconfig")
	if got := cfg.AdminKubeconfigPath(); got != want {
		t.Errorf("AdminKubeconfigPath() = %q, want %q", got, want)
	}
}
