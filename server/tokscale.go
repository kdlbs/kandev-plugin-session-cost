package main

import (
	"context"
	"strings"
)

// pinnedTokscale is the npx fallback used when tokscale is neither configured
// nor on PATH. Pinned so the JSON output shape can't drift under us; bump
// deliberately after checking `models --json` still parses.
const pinnedTokscale = "tokscale@4.15.1"

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

// InstallStatus reports the command used to build a successful report. It
// remains part of the response for compatibility with existing UI.
type InstallStatus struct {
	// Command is the resolved argv joined for display, e.g. "npx -y tokscale@4.15.1".
	Command string `json:"command"`
	// Source is where the command came from: "settings", "path" or "npx".
	Source string `json:"source"`
	// Installed is true when the report command completes successfully.
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

// runner executes a command and returns its stdout. Tests inject deterministic
// implementations; production uses the process-tree-aware runner.
type runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// commandDisplay renders the resolved argv for humans ("npx -y tokscale@4.15.1").
func commandDisplay(cmd resolvedCommand) string {
	return strings.Join(cmd.Argv, " ")
}
