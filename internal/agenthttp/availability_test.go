package agenthttp

import (
	"path/filepath"
	"testing"
)

func TestGetAgentAvailabilityReportsInstalledAndSupportedKnownAgents(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "codex", "#!/bin/sh\n")
	writeFakeCommand(t, binDir, "claude", "#!/bin/sh\n")

	agents, err := GetAgentAvailability([]AgentConfig{
		{Name: "codex", Command: "codex", Supported: true},
		{Name: "claude", Command: "claude", Supported: true},
	}, []string{"PATH=" + binDir})
	if err != nil {
		t.Fatal(err)
	}

	want := []AgentStatus{
		{Name: "codex", Command: "codex", Available: true, Supported: true},
		{Name: "claude", Command: "claude", Available: true, Supported: true},
	}

	if len(agents) != len(want) {
		t.Fatalf("len = %d, want %d", len(agents), len(want))
	}
	for i := range agents {
		if agents[i] != want[i] {
			t.Fatalf("agents[%d] = %#v, want %#v", i, agents[i], want[i])
		}
	}
}

func TestDefaultKnownAgentsOnlyIncludesSupportedRunners(t *testing.T) {
	agents := DefaultKnownAgents()
	want := []AgentConfig{
		{Name: "codex", Command: "codex", Supported: true},
		{Name: "claude", Command: "claude", Supported: true},
	}
	if len(agents) != len(want) {
		t.Fatalf("len = %d, want %d", len(agents), len(want))
	}
	for i := range agents {
		if agents[i] != want[i] {
			t.Fatalf("agents[%d] = %#v, want %#v", i, agents[i], want[i])
		}
	}
}

func TestFindExecutableReturnsExecutablePath(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "codex", "#!/bin/sh\n")

	path, err := FindExecutable("codex", []string{"PATH=" + binDir})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(binDir, "codex") {
		t.Fatalf("path = %q", path)
	}
}

func TestFindExecutableAcceptsAbsolutePath(t *testing.T) {
	binDir := t.TempDir()
	commandPath := filepath.Join(binDir, "codex")
	writeFakeCommand(t, binDir, "codex", "#!/bin/sh\n")

	path, err := FindExecutable(commandPath, []string{"PATH="})
	if err != nil {
		t.Fatal(err)
	}
	if path != commandPath {
		t.Fatalf("path = %q", path)
	}
}
