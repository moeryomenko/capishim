package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/moeryomenko/capishim/internal/config"
	"github.com/moeryomenko/capishim/internal/pki"
)

// runPKI executes the pki subcommand: it loads the runtime configuration from
// the process environment, ensures the full certificate inventory exists under
// <state-dir>/pki/ (REQ-002, REQ-009), and prints the artifact paths. It never
// contacts the apiserver, so it can run before etcd and kube-apiserver start.
func runPKI(ctx context.Context, stdout, stderr io.Writer, env map[string]string) int {
	cfg, err := config.Load(env)
	if err != nil {
		fmt.Fprintf(stderr, "capishim pki: load config: %v\n", err)
		return exitError
	}
	inv, err := pki.Generate(ctx, pki.Config{
		StateDir:              cfg.StateDir,
		BindAddress:           cfg.BindAddress,
		HypervisorWebhookHost: cfg.HypervisorWebhookHost,
	})
	if err != nil {
		fmt.Fprintf(stderr, "capishim pki: generate certificates: %v\n", err)
		return exitError
	}
	pkiDir := filepath.Dir(inv.CA.CertPath)
	fmt.Fprintf(stdout, "certificate inventory ready under %s:\n", pkiDir)
	for _, artifact := range inv.All() {
		fmt.Fprintf(stdout, "  %s: %s / %s\n", artifact.Name, artifact.CertPath, artifact.KeyPath)
	}
	fmt.Fprintf(stdout, "  service-account keypair: %s / %s\n", inv.SAPubPath, inv.SAKeyPath)
	return exitOK
}
