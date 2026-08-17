// The render-quadlet subcommand renders the quadlet unit graph for the current
// configuration and writes the units into a host-side directory. It is the
// rendering path behind make install-quadlet (REQ-001, REQ-011); the setup
// container never runs it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moeryomenko/capishim/internal/config"
	"github.com/moeryomenko/capishim/internal/quadlet"
)

// EnvImageVersion is the environment key for the image tag applied to
// capishim-built images when rendering quadlet units; it matches the Makefile
// CAPISHIM_VERSION variable (REQ-011).
const EnvImageVersion = "CAPISHIM_VERSION"

// unitFileMode is the permission of the rendered unit files on the host.
const unitFileMode os.FileMode = 0o644

// outputDirMode is the permission of the directory the units are written into.
const outputDirMode os.FileMode = 0o755

// runRenderQuadlet executes the render-quadlet subcommand: it loads the
// configuration from the environment, renders the full quadlet unit set, and
// writes one file per unit into --dir, overwriting any existing files. It
// returns the process exit code.
func runRenderQuadlet(stdout, stderr io.Writer, env map[string]string, args []string) int {
	fs := flag.NewFlagSet("render-quadlet", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("dir", "", "directory to write the rendered units into (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "capishim render-quadlet: unexpected arguments: %v\n", fs.Args())
		return exitUsage
	}
	if *outDir == "" {
		fmt.Fprintln(stderr, "capishim render-quadlet: --dir is required")
		return exitUsage
	}
	units, err := renderQuadlet(*outDir, env)
	if err != nil {
		fmt.Fprintf(stderr, "capishim render-quadlet: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "rendered %d quadlet units into %s\n", len(units), *outDir)
	return exitOK
}

// renderQuadlet renders the quadlet unit set from the environment
// configuration and writes one file per unit into dir with 0644 permissions,
// overwriting existing files. It returns the rendered units keyed by unit
// filename.
func renderQuadlet(dir string, env map[string]string) (map[string]string, error) {
	cfg, err := config.Load(env)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	units, err := quadlet.Render(quadlet.Input{
		Config:  cfg,
		Version: imageVersion(env),
	})
	if err != nil {
		return nil, fmt.Errorf("render quadlet units: %w", err)
	}
	if err := os.MkdirAll(dir, outputDirMode); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(units[name]), unitFileMode); err != nil {
			return nil, fmt.Errorf("write unit %s: %w", name, err)
		}
	}
	return units, nil
}

// imageVersion resolves the image tag for capishim-built images from the
// environment: EnvImageVersion when set and non-empty, otherwise the empty
// string, which keeps the component table's default tag (REQ-011).
func imageVersion(env map[string]string) string {
	return strings.TrimSpace(env[EnvImageVersion])
}
