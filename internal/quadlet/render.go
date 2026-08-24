// Package quadlet renders the quadlet unit files that define the capishim
// pod: one pod unit and one container unit per component in the config table.
// The rendered units are the sources installed by make install-quadlet, so
// the pod network namespace, publish port, dependency chain, image
// references, volume mounts, environment, and manager Exec lines (REQ-001,
// REQ-006, REQ-009, REQ-010, REQ-011) all come from this package.
package quadlet

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/moeryomenko/capishim/internal/config"
	quadletassets "github.com/moeryomenko/capishim/quadlet"
)

// Unit filename and image reference constants shared by the renderer.
const (
	// podUnitName is the rendered pod unit filename.
	podUnitName = "capishim.pod"

	// unitPrefix is the filename prefix shared by every container unit.
	unitPrefix = "capishim-"

	// containerSuffix is the filename suffix of every container unit.
	containerSuffix = ".container"

	// containerPort is the apiserver port inside the pod; the pod always
	// publishes it regardless of the host-side bind port (REQ-010).
	containerPort = "6443"

	// etcdClientPort and etcdPeerPort are the fixed pod-internal etcd ports
	// used by the etcd Exec line (mirrors e2e/shim/components.go).
	etcdClientPort = "2379"
	etcdPeerPort   = "2380"

	// localImagePrefix identifies capishim-built images whose tag the
	// renderer rewrites to the requested version.
	localImagePrefix = "localhost/capishim-"
)

// Input is the configuration the renderer needs: the resolved config plus the
// version tag applied to capishim-built images.
type Input struct {
	// Config carries the state directory and apiserver bind address.
	Config config.Config
	// Version is the image tag for localhost/capishim-* images; empty keeps
	// the table's default tag.
	Version string
}

// templateData is the substitution data for one unit template.
type templateData struct {
	// PublishPort is the pod's PublishPort value: the bind address plus the
	// fixed container port.
	PublishPort string
	// Image is the container image reference for the unit.
	Image string
	// StateDir is the state directory root used in volume mounts.
	StateDir string
	// BindAddress is the apiserver bind address passed to the setup
	// container.
	BindAddress string
	// EnvStateDir is the CAPISHIM_STATE_DIR environment key.
	EnvStateDir string
	// EnvBindAddr is the CAPISHIM_BIND_ADDRESS environment key.
	EnvBindAddr string
	// Exec is the container command line; empty for units without one.
	Exec string
}

// Render renders the full quadlet unit set: the pod unit plus one container
// unit per in-pod component in the config table. Specs with External=true
// have no container unit (REQ-004). The result is keyed by unit filename and
// is deterministic for a given input.
func Render(in Input) (map[string]string, error) {
	if err := validateInput(in); err != nil {
		return nil, err
	}
	units := make(map[string]string)
	pod, err := RenderUnit(podUnitName, in)
	if err != nil {
		return nil, err
	}
	units[podUnitName] = pod
	for _, spec := range config.Components() {
		if spec.External {
			// External managers boot from their own quadlet units outside the
			// pod; rendering a capishim-<id>.container for them would
			// double-boot the provider (REQ-004).
			continue
		}
		name := unitFileName(spec.ID)
		unit, err := RenderUnit(name, in)
		if err != nil {
			return nil, fmt.Errorf("quadlet: render %q: %w", name, err)
		}
		units[name] = unit
	}
	return units, nil
}

// RenderUnit renders the single unit with the given filename. Unknown names
// and inputs with an empty state directory or bind address are errors.
func RenderUnit(name string, in Input) (string, error) {
	if err := validateInput(in); err != nil {
		return "", err
	}
	if name == podUnitName {
		return executeTemplate(name, templateData{
			PublishPort: in.Config.BindAddress + ":" + containerPort,
		})
	}
	id, ok := componentIDFromUnitName(name)
	if !ok {
		return "", fmt.Errorf("quadlet: unknown unit %q", name)
	}
	spec, ok := config.Component(id)
	if !ok {
		return "", fmt.Errorf("quadlet: unknown unit %q", name)
	}
	if spec.External {
		return "", fmt.Errorf("quadlet: %q is an external component and has no quadlet unit (REQ-004)", name)
	}
	return executeTemplate(unitFileName(spec.ID), unitDataFor(spec, in))
}

// validateInput rejects inputs that would render malformed units: the state
// directory and bind address must be non-empty.
func validateInput(in Input) error {
	if strings.TrimSpace(in.Config.StateDir) == "" {
		return errors.New("quadlet: Config.StateDir must not be empty")
	}
	if strings.TrimSpace(in.Config.BindAddress) == "" {
		return errors.New("quadlet: Config.BindAddress must not be empty")
	}
	return nil
}

// componentIDFromUnitName parses a container unit filename into the component
// ID it renders. The bool is false for names that are not
// capishim-<id>.container.
func componentIDFromUnitName(name string) (config.ComponentID, bool) {
	if !strings.HasPrefix(name, unitPrefix) || !strings.HasSuffix(name, containerSuffix) {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, unitPrefix), containerSuffix)
	if id == "" {
		return "", false
	}
	return config.ComponentID(id), true
}

// unitFileName is the rendered unit filename for the component with the given
// ID.
func unitFileName(id config.ComponentID) string {
	return unitPrefix + string(id) + containerSuffix
}

// unitDataFor builds the template data for one component: the rewritten image
// reference, the volume mount roots, and the Exec command line. Every
// component carries an Exec: etcd and apiserver run their binaries, managers
// run the provider binaries, and the pki/setup oneshot containers dispatch
// their entrypoint subcommand (pki and setup). Podman quadlet appends Exec=
// to the image ENTRYPOINT, so the bare subcommand is all that is needed.
func unitDataFor(spec config.ComponentSpec, in Input) templateData {
	data := templateData{
		Image:       imageFor(spec.Image, in.Version),
		StateDir:    in.Config.StateDir,
		BindAddress: in.Config.BindAddress,
		EnvStateDir: config.EnvStateDir,
		EnvBindAddr: config.EnvBindAddress,
	}
	switch spec.ID {
	case config.ComponentEtcd:
		data.Exec = etcdExec(in.Config.StateDir)
	case config.ComponentAPIServer:
		data.Exec = apiserverExec(in.Config.StateDir)
	case config.ComponentCore, config.ComponentCABPK, config.ComponentKCP, config.ComponentCAPD:
		data.Exec = managerExec(spec, in.Config.StateDir)
	case config.ComponentPKI:
		data.Exec = "pki"
	case config.ComponentSetup:
		data.Exec = "setup"
	}
	return data
}

// imageFor returns the Image= reference for a component: the table image
// verbatim when Version is empty, and with the tag replaced by Version for
// localhost/capishim-* references. Stock references are never rewritten.
func imageFor(ref, version string) string {
	if version == "" || !strings.HasPrefix(ref, localImagePrefix) {
		return ref
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return ref
	}
	return ref[:i] + ":" + version
}

// etcdExec builds the etcd Exec line: a single-node TLS cluster on loopback
// with the server certificate minted by the pki container, client and peer
// traffic both authenticated (mirrors e2e/shim/components.go).
func etcdExec(stateDir string) string {
	return strings.Join([]string{
		"etcd",
		"--name=capishim-etcd",
		"--data-dir=" + filepath.Join(stateDir, "etcd"),
		"--listen-client-urls=https://127.0.0.1:" + etcdClientPort,
		"--advertise-client-urls=https://127.0.0.1:" + etcdClientPort,
		"--listen-peer-urls=https://127.0.0.1:" + etcdPeerPort,
		"--initial-advertise-peer-urls=https://127.0.0.1:" + etcdPeerPort,
		"--initial-cluster=capishim-etcd=https://127.0.0.1:" + etcdPeerPort,
		"--cert-file=" + filepath.Join(stateDir, "pki", "etcd-server.crt"),
		"--key-file=" + filepath.Join(stateDir, "pki", "etcd-server.key"),
		"--trusted-ca-file=" + filepath.Join(stateDir, "pki", "ca.crt"),
		"--client-cert-auth=true",
		"--peer-cert-file=" + filepath.Join(stateDir, "pki", "etcd-server.crt"),
		"--peer-key-file=" + filepath.Join(stateDir, "pki", "etcd-server.key"),
		"--peer-trusted-ca-file=" + filepath.Join(stateDir, "pki", "ca.crt"),
		"--peer-client-cert-auth=true",
	}, " ")
}

// apiserverExec builds the kube-apiserver Exec line: etcd TLS client certs,
// the serving cert, SA signing keypair, ABAC+RBAC authorization with the
// bootstrap policy file, and a pod-wide bind so rootless podman port
// publishing can reach it (mirrors e2e/shim/components.go).
func apiserverExec(stateDir string) string {
	return strings.Join([]string{
		"kube-apiserver",
		"--etcd-servers=https://127.0.0.1:" + etcdClientPort,
		"--etcd-cafile=" + filepath.Join(stateDir, "pki", "ca.crt"),
		"--etcd-certfile=" + filepath.Join(stateDir, "pki", "apiserver-client.crt"),
		"--etcd-keyfile=" + filepath.Join(stateDir, "pki", "apiserver-client.key"),
		"--client-ca-file=" + filepath.Join(stateDir, "pki", "ca.crt"),
		"--tls-cert-file=" + filepath.Join(stateDir, "pki", "apiserver.crt"),
		"--tls-private-key-file=" + filepath.Join(stateDir, "pki", "apiserver.key"),
		"--service-account-key-file=" + filepath.Join(stateDir, "pki", "sa.pub"),
		"--service-account-signing-key-file=" + filepath.Join(stateDir, "pki", "sa.key"),
		"--service-account-issuer=https://127.0.0.1:" + containerPort,
		"--authorization-mode=ABAC,RBAC",
		"--authorization-policy-file=" + filepath.Join(stateDir, "abac", "policy.json"),
		"--bind-address=0.0.0.0",
		"--secure-port=" + containerPort,
		"--service-cluster-ip-range=10.128.0.0/12",
		"--allow-privileged=true",
	}, " ")
}

// managerExec builds the manager Exec line: the kubeconfig, webhook port and
// cert directory, per-manager health and diagnostics listeners, and the
// ClusterTopology feature gate on every manager (REQ-006 as corrected by the
// e2e proof). No leader-election flags: the v1.14 binaries reject
// --leader-election-namespace and controller-runtime cannot default an
// election namespace outside a cluster.
func managerExec(spec config.ComponentSpec, stateDir string) string {
	return strings.Join([]string{
		"--kubeconfig=" + filepath.Join(stateDir, spec.Kubeconfig),
		"--webhook-port=" + strconv.Itoa(spec.WebhookPort),
		"--webhook-cert-dir=" + filepath.Join(stateDir, "pki", string(spec.ID)+"-webhook"),
		"--health-addr=127.0.0.1:" + strconv.Itoa(spec.HealthPort),
		"--diagnostics-address=127.0.0.1:" + strconv.Itoa(spec.DiagnosticsPort),
		"--feature-gates=ClusterTopology=true",
	}, " ")
}

// executeTemplate loads, parses, and executes the unit template with the
// given name, returning the rendered unit text.
func executeTemplate(name string, data templateData) (string, error) {
	raw, err := quadletassets.UnitTemplate(name)
	if err != nil {
		return "", fmt.Errorf("quadlet: load template %q: %w", name, err)
	}
	t, err := template.New(name).Parse(raw)
	if err != nil {
		return "", fmt.Errorf("quadlet: parse template %q: %w", name, err)
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("quadlet: execute template %q: %w", name, err)
	}
	return buf.String(), nil
}
