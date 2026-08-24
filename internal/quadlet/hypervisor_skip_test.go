// Quadlet renderer skip-path tests (TASK-008 red phase). REQ-004 requires
// render-quadlet to emit NO unit for any component spec with External=true:
// the hypervisor manager is booted by its own CAPH quadlet unit (REQ-007), so
// a capishim-<id>.container unit for it would double-boot the provider. The
// eight existing units and the pod unit must be unaffected.
//
// Until TASK-009 adds config.ComponentHypervisor and ComponentSpec.External
// this package fails to compile; that build failure is the red phase.
package quadlet_test

import (
	"testing"

	"github.com/moeryomenko/capishim/internal/config"
	"github.com/moeryomenko/capishim/internal/quadlet"
)

// inPodComponentIDs lists the eight components that keep their container
// units: everything in the table except the external hypervisor manager.
var inPodComponentIDs = []config.ComponentID{
	config.ComponentPKI,
	config.ComponentEtcd,
	config.ComponentAPIServer,
	config.ComponentSetup,
	config.ComponentCore,
	config.ComponentCABPK,
	config.ComponentKCP,
	config.ComponentCAPD,
}

// TestRenderSkipsExternalHypervisor proves the renderer skip-path: the
// hypervisor spec exists in the table with External=true, yet Render emits no
// capishim-hypervisor.container and the pre-existing nine-unit set (pod plus
// eight containers) is unchanged.
func TestRenderSkipsExternalHypervisor(t *testing.T) {
	t.Parallel()
	spec, ok := config.Component(config.ComponentHypervisor)
	if !ok {
		t.Fatalf("config.Component(%q) not found: the skip path needs the table entry to exist before it can be exercised (REQ-004)", config.ComponentHypervisor)
	}
	if !spec.External {
		t.Fatalf("hypervisor spec External = false, want true: only External specs are skipped")
	}

	units := renderWith(t, testVersion, testBind)

	hypervisorUnit := "capishim-" + string(config.ComponentHypervisor) + ".container"
	if _, emitted := units[hypervisorUnit]; emitted {
		t.Errorf("Render emitted %s for an External spec; render-quadlet must not produce a unit for it (REQ-004)\n---\n%s\n---", hypervisorUnit, units[hypervisorUnit])
	}
	for _, id := range inPodComponentIDs {
		name := "capishim-" + string(id) + ".container"
		if _, ok := units[name]; !ok {
			t.Errorf("Render output missing existing unit %q: skipping External specs must not disturb the eight in-pod units", name)
		}
	}
	if _, ok := units["capishim.pod"]; !ok {
		t.Error("Render output missing capishim.pod")
	}
	if got := len(units); got != 9 {
		t.Errorf("Render returned %d units, want 9 (pod + eight in-pod containers, no hypervisor unit)", got)
	}
}

// TestRenderUnitRejectsHypervisorUnitName pins the single-unit seam to the
// same contract: after the skip path lands there is no template data for an
// external component, so RenderUnit must reject its unit name instead of
// rendering a stray unit.
func TestRenderUnitRejectsHypervisorUnitName(t *testing.T) {
	t.Parallel()
	in := quadlet.Input{Config: config.Config{StateDir: testStateDir, BindAddress: testBind}, Version: testVersion}
	name := "capishim-" + string(config.ComponentHypervisor) + ".container"
	if unit, err := quadlet.RenderUnit(name, in); err == nil {
		t.Errorf("RenderUnit(%q) returned no error and rendered:\n%s\n---\nwant an error: external components have no quadlet unit (REQ-004)", name, unit)
	}
}
