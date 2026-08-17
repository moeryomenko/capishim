package shim

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runPodman executes podman with args and returns its trimmed stdout. On a
// non-zero exit the error carries podman's stderr (or the wrapped exec error
// when podman produced no output), so callers can surface the exact driver
// failure.
func runPodman(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("podman %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runPodmanQuiet executes podman ignoring errors, for idempotent cleanup
// steps where a missing object is the expected state.
func runPodmanQuiet(ctx context.Context, args ...string) {
	_, _ = runPodman(ctx, args...)
}
