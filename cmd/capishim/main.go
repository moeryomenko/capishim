// Command capishim is the setup container entrypoint for the capishim
// management stack.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

// Exit codes returned by the capishim entrypoint.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, environMap()))
}

// environMap converts os.Environ's KEY=VALUE slice into a map for config.Load.
func environMap() map[string]string {
	env := make(map[string]string, len(os.Environ()))
	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			env[key] = value
		}
	}
	return env
}

// run dispatches the capishim entrypoint: the pki, setup, render-quadlet, and
// version subcommands, plus the legacy -version flag. It returns the process
// exit code.
func run(args []string, stdout, stderr io.Writer, env map[string]string) int {
	if len(args) > 0 {
		switch args[0] {
		case "pki":
			return runPKI(context.Background(), stdout, stderr, env)
		case "setup":
			return runSetup(context.Background(), stdout, stderr, env)
		case "render-quadlet":
			return runRenderQuadlet(stdout, stderr, env, args[1:])
		case "version":
			return runVersion(stdout)
		}
	}

	fs := flag.NewFlagSet("capishim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if *showVersion {
		return runVersion(stdout)
	}
	return exitOK
}

// runVersion prints the module version of the running binary to stdout.
func runVersion(stdout io.Writer) int {
	fmt.Fprintln(stdout, "capishim "+moduleVersion())
	return exitOK
}

// moduleVersion returns the version embedded in the binary at build time,
// falling back to "dev" for local builds that carry no version information.
func moduleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
