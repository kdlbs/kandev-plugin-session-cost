package main

import (
	"context"
	"strings"
)

// pinnedTokscale is the npx fallback used when tokscale is neither configured
// nor on PATH. Pinned so the JSON output shape can't drift under us; bump
// deliberately after checking `models --json` still parses.
const pinnedTokscale = "tokscale@4.5.3"

// Command sources, reported in InstallStatus.Source so the UI can explain
// where the tokscale invocation came from.
const (
	sourceSettings = "settings" // operator-configured command
	sourcePath     = "path"     // tokscale binary found on PATH
	sourceNpx      = "npx"      // pinned npx fallback
)

// resolvedCommand is the argv the plugin will run tokscale with, plus where
// that argv came from.
type resolvedCommand struct {
	Argv   []string
	Source string
}

// resolveCommand picks the tokscale invocation: the operator-configured
// command wins, then a `tokscale` binary on PATH, then the pinned npx
// fallback. lookPath is exec.LookPath in production, injected for tests.
func resolveCommand(configured string, lookPath func(string) (string, error)) resolvedCommand {
	if argv := strings.Fields(configured); len(argv) > 0 {
		return resolvedCommand{Argv: argv, Source: sourceSettings}
	}
	if path, err := lookPath("tokscale"); err == nil {
		return resolvedCommand{Argv: []string{path}, Source: sourcePath}
	}
	return resolvedCommand{Argv: []string{"npx", "-y", pinnedTokscale}, Source: sourceNpx}
}

// InstallStatus reports whether the resolved command actually runs, and as
// what version. Embedded in every session-cost payload so the UI can render
// setup guidance from the same shape it always reads.
type InstallStatus struct {
	// Command is the resolved argv joined for display, e.g. "npx -y tokscale@4.5.3".
	Command string `json:"command"`
	// Source is where the command came from: "settings", "path" or "npx".
	Source    string `json:"source"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

// runner executes a command and returns its stdout — exec.CommandContext in
// production (see newPlugin), injected for tests.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// commandDisplay renders the resolved argv for humans ("npx -y tokscale@4.5.3").
func commandDisplay(cmd resolvedCommand) string {
	return strings.Join(cmd.Argv, " ")
}

// probeInstall checks the resolved command works by running `--version` and
// parsing the version out of its output ("tokscale 4.5.3").
func probeInstall(ctx context.Context, cmd resolvedCommand, run runner) InstallStatus {
	status := InstallStatus{Command: commandDisplay(cmd), Source: cmd.Source}
	out, err := run(ctx, cmd.Argv[0], append(cmd.Argv[1:], "--version")...)
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Installed = true
	status.Version = parseVersion(string(out))
	return status
}

// parseVersion extracts "4.5.3" from tokscale's `--version` output, which may
// be preceded by npx install noise. Empty when no version line is found.
func parseVersion(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "tokscale" {
			return fields[1]
		}
	}
	return ""
}
