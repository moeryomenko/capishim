// Package quadlet hosts the templated quadlet unit sources that the capishim
// renderer (github.com/moeryomenko/capishim/internal/quadlet) consumes: one
// template per rendered unit, kept here so the unit sources live at the repo
// root under quadlet/ (REQ-001) and can be embedded into the renderer
// binary.
package quadlet

import (
	"embed"
	"fmt"
)

// unitTemplates embeds the quadlet unit sources rendered by the capishim
// renderer: one template per rendered unit, keyed by unit filename
// (REQ-001).
//
//go:embed capishim.pod capishim-*.container
var unitTemplates embed.FS

// UnitTemplate returns the embedded unit template with the given unit
// filename. Unknown names are errors.
func UnitTemplate(name string) (string, error) {
	raw, err := unitTemplates.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("quadlet: read unit template %q: %w", name, err)
	}
	return string(raw), nil
}
