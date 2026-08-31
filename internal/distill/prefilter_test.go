package distill

import (
	"testing"

	"github.com/hoklims/temper/internal/canon"
)

func sess(turns ...canon.Turn) *canon.Session {
	return &canon.Session{Turns: turns}
}

func TestPrefilterSkipsNoiseSession(t *testing.T) {
	s := sess(
		canon.Turn{Role: canon.RoleUser, Text: "can you add a hello world function"},
		canon.Turn{Role: canon.RoleAssistant, Text: "Sure, here is a function that prints hello."},
	)
	kinds, worth := HasSignal(s)
	if worth {
		t.Errorf("expected noise session to be skipped, got kinds=%v", kinds)
	}
}

func TestPrefilterCatchesCorrection(t *testing.T) {
	s := sess(
		canon.Turn{Role: canon.RoleUser, Text: "no, actually we use pnpm here, not npm"},
		canon.Turn{Role: canon.RoleAssistant, Text: "Understood, switching to pnpm."},
	)
	kinds, worth := HasSignal(s)
	if !worth {
		t.Fatalf("expected correction to be worth distilling")
	}
	if !contains(kinds, "correction") || !contains(kinds, "convention") {
		t.Errorf("expected correction+convention kinds, got %v", kinds)
	}
}

func TestPrefilterCatchesFailure(t *testing.T) {
	s := sess(
		canon.Turn{Role: canon.RoleUser, Text: "run the tests"},
		canon.Turn{Role: canon.RoleTool, Text: "ModuleNotFoundError: No module named 'foo'\nTraceback (most recent call last):"},
		canon.Turn{Role: canon.RoleAssistant, Text: "Installing the missing dependency fixes it."},
	)
	if _, worth := HasSignal(s); !worth {
		t.Errorf("expected failure_fix session to be worth distilling")
	}
}

func TestPrefilterCatchesRepeatedCommand(t *testing.T) {
	s := sess(
		canon.Turn{Role: canon.RoleAssistant, Text: "let me run\n$ go test ./...\n"},
		canon.Turn{Role: canon.RoleAssistant, Text: "trying again\n$ go test ./...\n"},
	)
	kinds, worth := HasSignal(s)
	if !worth || !contains(kinds, "rediscovery") {
		t.Errorf("expected rediscovery from repeated command, got kinds=%v worth=%v", kinds, worth)
	}
}

func TestPrefilterCatchesWorkflowFromStepMarkers(t *testing.T) {
	s := sess(
		canon.Turn{Role: canon.RoleUser, Text: "document how to deploy"},
		canon.Turn{Role: canon.RoleAssistant, Text: "Step 1: build the image. Step 2: push it. After that, run the migration."},
	)
	kinds, worth := HasSignal(s)
	if !worth || !contains(kinds, "workflow") {
		t.Errorf("expected workflow from step markers, got kinds=%v worth=%v", kinds, worth)
	}
}

func TestPrefilterCatchesWorkflowFromDistinctCommands(t *testing.T) {
	s := sess(
		canon.Turn{Role: canon.RoleAssistant, Text: "$ pnpm install\n$ pnpm build\n$ docker compose up\n"},
	)
	kinds, worth := HasSignal(s)
	if !worth || !contains(kinds, "workflow") {
		t.Errorf("expected workflow from 3+ distinct commands, got kinds=%v worth=%v", kinds, worth)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
