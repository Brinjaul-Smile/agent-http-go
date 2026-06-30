package agenthttp

import (
	"path/filepath"
	"testing"
)

func TestGetAgentAvailabilityReportsInstalledAndSupportedKnownAgents(t *testing.T) {
	binDir := t.TempDir()
	writeFakeCommand(t, binDir, "codex", "#!/bin/sh\n")
	writeFakeCommand(t, binDir, "gemini", "#!/bin/sh\n")

	agents, err := GetAgentAvailability([]AgentConfig{
		{Name: "codex", Command: "codex", Supported: true},
		{Name: "gemini", Command: "gemini", Supported: false},
		{Name: "opencode", Command: "opencode", Supported: false},
	}, []string{"PATH=" + binDir})
	if err != nil {
		t.Fatal(err)
	}

	want := []AgentStatus{
		{Name: "codex", Command: "codex", Available: true, Supported: true},
		{Name: "gemini", Command: "gemini", Available: true, Supported: false},
		{Name: "opencode", Command: "opencode", Available: false, Supported: false, Error: "opencode CLI not found in PATH"},
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

func TestDefaultKnownAgentsIncludesPiCodingAgent(t *testing.T) {
	found := false
	for _, agent := range DefaultKnownAgents() {
		if agent.Name == "pi" {
			found = true
			if agent.Command != "pi" {
				t.Fatalf("command = %q, want pi", agent.Command)
			}
			if agent.Supported {
				t.Fatal("pi supported = true, want false")
			}
		}
	}
	if !found {
		t.Fatal("pi agent not found")
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
