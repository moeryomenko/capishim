// Package quadlet renders the quadlet unit files that define the capishim
// pod: one pod unit and one container unit per component in the config table.
// The rendered units are the sources installed by make install-quadlet, so
// the pod network namespace, publish port, dependency chain, image
// references, volume mounts, environment, and manager command flags (REQ-001,
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
	// Command is the manager command line; empty for non-manager units.
	Command string
}

// Render renders the full quadlet unit set: the pod unit plus one container
// unit per component in the config table. The result is keyed by unit
// filename and is deterministic for a given input.
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
// reference, the volume mount roots, and for manager components the command
// line.
func unitDataFor(spec config.ComponentSpec, in Input) templateData {
	data := templateData{
		Image:       imageFor(spec.Image, in.Version),
		StateDir:    in.Config.StateDir,
		BindAddress: in.Config.BindAddress,
		EnvStateDir: config.EnvStateDir,
		EnvBindAddr: config.EnvBindAddress,
	}
	if spec.WebhookPort != 0 {
		data.Command = commandFor(spec, in.Config.StateDir)
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

// commandFor builds the manager command line: the kubeconfig path, the
// component's webhook port and cert directory, leader election flags, and for
// the core manager the ClusterTopology feature gate (REQ-006).
func commandFor(spec config.ComponentSpec, stateDir string) string {
	var b strings.Builder
	b.WriteString("--kubeconfig=")
	b.WriteString(filepath.Join(stateDir, spec.Kubeconfig))
	b.WriteString(" --webhook-port=")
	b.WriteString(strconv.Itoa(spec.WebhookPort))
	b.WriteString(" --webhook-cert-dir=")
	b.WriteString(filepath.Join(stateDir, "pki", string(spec.ID)+"-webhook"))
	b.WriteString(" --leader-elect --leader-election-namespace=")
	b.WriteString(spec.ProviderNamespace)
	if spec.ID == config.ComponentCore {
		b.WriteString(" --feature-gates=ClusterTopology=true")
	}
	return b.String()
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
