package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hoklims/temper/internal/migrate"
	"github.com/hoklims/temper/internal/store"
)

func setTestHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func TestMigrateStateReconcilesBeforeHomeMove(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	legacy := filepath.Join(home, migrate.LegacyDirName)
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(legacy, migrate.LegacyDBName))
	if err != nil {
		t.Fatal(err)
	}
	suggestion := store.Suggestion{
		ID: "sg_legacy_journal", CreatedAt: time.Now(), Status: "pending", Title: "legacy",
		Signal: "convention", Scope: "machine", Placement: "skill", Confidence: 0.9,
		Body: "- keep legacy state recoverable",
	}
	if err := st.InsertSuggestion(suggestion); err != nil {
		t.Fatal(err)
	}
	legacyTarget := filepath.Join(legacy, "skills", "legacy.md")
	op := store.Operation{
		ID: store.NewOperationID(), SuggestionID: suggestion.ID, Kind: "accept",
		Manifest: "prepared manifests are intentionally not parsed", TargetStatus: "accepted",
		TargetPath: legacyTarget,
	}
	if err := st.BeginOperation(op, "pending", []string{legacyTarget}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	result, _, err := migrateState()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(home, migrate.CurrentDirName, migrate.CurrentDBName)); err != nil {
		t.Fatalf("migrated database missing: %v", err)
	}
	migrated, err := store.Open(filepath.Join(home, migrate.CurrentDirName, migrate.CurrentDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if open, err := migrated.IncompleteOperations(); err != nil || len(open) != 0 {
		t.Fatalf("legacy journal was moved before reconciliation: open=%+v err=%v", open, err)
	}
	got, err := migrated.GetSuggestion(suggestion.ID)
	if err != nil || got.Status != "pending" {
		t.Fatalf("prepared legacy decision changed during migration: status=%q err=%v", got.Status, err)
	}
}

func TestInstallSystemdHealthFailureRestoresLegacyServiceAndHome(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyUnit := filepath.Join(unitDir, legacyServiceName)
	legacyBytes := []byte("legacy unit")
	if err := os.WriteFile(legacyUnit, legacyBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, migrate.LegacyDirName), 0o700); err != nil {
		t.Fatal(err)
	}

	original := runServiceCommand
	originalActive := serviceActive
	t.Cleanup(func() {
		runServiceCommand = original
		serviceActive = originalActive
	})
	var calls []string
	runServiceCommand = func(name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		return nil, nil
	}
	serviceActive = func(name string, args ...string) (bool, error) {
		if args[len(args)-1] == legacyServiceName {
			return false, nil
		}
		return false, nil
	}
	if err := installSystemd(false); err == nil || !strings.Contains(err.Error(), "health") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, migrate.LegacyDirName)); err != nil {
		t.Fatalf("legacy home was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, migrate.CurrentDirName)); !os.IsNotExist(err) {
		t.Fatalf("current home remains after rollback: %v", err)
	}
	got, err := os.ReadFile(legacyUnit)
	if err != nil || !reflect.DeepEqual(got, legacyBytes) {
		t.Fatalf("legacy unit not restored: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(unitDir, serviceName)); !os.IsNotExist(err) {
		t.Fatalf("new unit remains after rollback: %v", err)
	}
	if calls[len(calls)-1] != "systemctl --user enable --now "+legacyServiceName {
		t.Fatalf("legacy unit was not restarted last: %v", calls)
	}
}

func TestInstallLaunchdHealthFailureRestoresLegacyAgentAndHome(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPlist := filepath.Join(agents, legacyLaunchdLabel+".plist")
	if err := os.WriteFile(legacyPlist, []byte("legacy plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, migrate.LegacyDirName), 0o700); err != nil {
		t.Fatal(err)
	}

	original := runServiceCommand
	originalActive := serviceActive
	t.Cleanup(func() {
		runServiceCommand = original
		serviceActive = originalActive
	})
	runServiceCommand = func(name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	serviceActive = func(name string, args ...string) (bool, error) { return false, nil }
	if err := installLaunchd(false); err == nil || !strings.Contains(err.Error(), "health") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, migrate.LegacyDirName)); err != nil {
		t.Fatalf("legacy home was not restored: %v", err)
	}
	if got, err := os.ReadFile(legacyPlist); err != nil || string(got) != "legacy plist" {
		t.Fatalf("legacy plist not restored: %q, %v", got, err)
	}
}

func TestUninstallSystemdDoesNotRequireMigration(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	for _, name := range []string{migrate.LegacyDirName, migrate.CurrentDirName} {
		if err := os.Mkdir(filepath.Join(home, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	original := runServiceCommand
	originalActive := serviceActive
	t.Cleanup(func() {
		runServiceCommand = original
		serviceActive = originalActive
	})
	runServiceCommand = func(string, ...string) ([]byte, error) { return nil, nil }
	if err := installSystemd(true); err != nil {
		t.Fatalf("uninstall was blocked by migration conflict: %v", err)
	}
}

func TestLaunchdProcessRunningRequiresPID(t *testing.T) {
	if launchdProcessRunning([]byte(`{"Label" = "io.temper.daemon"; "LastExitStatus" = 1;}`)) {
		t.Fatal("a registered but crashed launchd job was treated as healthy")
	}
	if !launchdProcessRunning([]byte(`{"PID" = 4242; "Label" = "io.temper.daemon";}`)) {
		t.Fatal("a launchd job with an active PID was not recognized")
	}
}

func TestServiceStableRequiresTwoHealthyChecks(t *testing.T) {
	originalActive, originalWait := serviceActive, waitForServiceStability
	t.Cleanup(func() {
		serviceActive = originalActive
		waitForServiceStability = originalWait
	})
	checks := 0
	serviceActive = func(string, ...string) (bool, error) {
		checks++
		return checks == 1, nil
	}
	waitForServiceStability = func(time.Duration) {}
	if stable, err := serviceStable("service"); err != nil || stable {
		t.Fatalf("stable=%t err=%v", stable, err)
	}
	if checks != 2 {
		t.Fatalf("health checks = %d", checks)
	}
}

func TestSystemdRollbackDoesNotTouchStateWhenTemperStopFails(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, legacyServiceName), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, migrate.LegacyDirName), 0o700); err != nil {
		t.Fatal(err)
	}

	originalRun, originalActive := runServiceCommand, serviceActive
	t.Cleanup(func() { runServiceCommand, serviceActive = originalRun, originalActive })
	restartedLegacy := false
	runServiceCommand = func(name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		if call == "systemctl --user disable --now "+serviceName {
			return []byte("busy"), errors.New("stop failed")
		}
		if call == "systemctl --user enable --now "+legacyServiceName {
			restartedLegacy = true
		}
		return nil, nil
	}
	serviceActive = func(name string, args ...string) (bool, error) { return false, nil }
	if err := installSystemd(false); err == nil || !strings.Contains(err.Error(), "rollback halted") {
		t.Fatalf("error = %v", err)
	}
	if restartedLegacy {
		t.Fatal("legacy service restarted without proving Temper stopped")
	}
	if _, err := os.Stat(filepath.Join(home, migrate.CurrentDirName)); err != nil {
		t.Fatalf("current home was rolled back while Temper may be running: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, migrate.LegacyDirName)); !os.IsNotExist(err) {
		t.Fatalf("legacy home was restored while Temper may be running: %v", err)
	}
}

func TestSystemdRollbackDoesNotTouchStateWhenTemperRemainsActive(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, legacyServiceName), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, migrate.LegacyDirName), 0o700); err != nil {
		t.Fatal(err)
	}

	originalRun, originalActive := runServiceCommand, serviceActive
	t.Cleanup(func() { runServiceCommand, serviceActive = originalRun, originalActive })
	restartedLegacy := false
	runServiceCommand = func(name string, args ...string) ([]byte, error) {
		if name+" "+strings.Join(args, " ") == "systemctl --user enable --now "+legacyServiceName {
			restartedLegacy = true
		}
		return nil, nil
	}
	newChecks := 0
	serviceActive = func(name string, args ...string) (bool, error) {
		if args[len(args)-1] == legacyServiceName {
			return false, nil
		}
		newChecks++
		return newChecks > 1, nil
	}
	if err := installSystemd(false); err == nil || !strings.Contains(err.Error(), "still active") {
		t.Fatalf("error = %v", err)
	}
	if restartedLegacy {
		t.Fatal("legacy service restarted while Temper was still active")
	}
	if _, err := os.Stat(filepath.Join(home, migrate.CurrentDirName)); err != nil {
		t.Fatalf("current home was rolled back while Temper was active: %v", err)
	}
}

func TestLaunchdRollbackDoesNotTouchStateWhenTemperStopFails(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPlist := filepath.Join(agents, legacyLaunchdLabel+".plist")
	if err := os.WriteFile(legacyPlist, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, migrate.LegacyDirName), 0o700); err != nil {
		t.Fatal(err)
	}

	originalRun, originalActive := runServiceCommand, serviceActive
	t.Cleanup(func() { runServiceCommand, serviceActive = originalRun, originalActive })
	restartedLegacy := false
	newPlist := filepath.Join(agents, launchdLabel+".plist")
	runServiceCommand = func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" && len(args) == 2 && args[0] == "unload" && filepath.Clean(args[1]) == filepath.Clean(newPlist) {
			return []byte("busy"), errors.New("stop failed")
		}
		if name == "launchctl" && len(args) == 2 && args[0] == "load" && filepath.Clean(args[1]) == filepath.Clean(legacyPlist) {
			restartedLegacy = true
		}
		return nil, nil
	}
	serviceActive = func(string, ...string) (bool, error) { return false, nil }
	if err := installLaunchd(false); err == nil || !strings.Contains(err.Error(), "rollback halted") {
		t.Fatalf("error = %v", err)
	}
	if restartedLegacy {
		t.Fatal("legacy launchd agent restarted without proving Temper stopped")
	}
	if _, err := os.Stat(filepath.Join(home, migrate.CurrentDirName)); err != nil {
		t.Fatalf("current home was rolled back while Temper may be running: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, migrate.LegacyDirName)); !os.IsNotExist(err) {
		t.Fatalf("legacy home was restored while Temper may be running: %v", err)
	}
}

func TestLaunchdRollbackDoesNotTouchStateWhenTemperRemainsActive(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPlist := filepath.Join(agents, legacyLaunchdLabel+".plist")
	if err := os.WriteFile(legacyPlist, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, migrate.LegacyDirName), 0o700); err != nil {
		t.Fatal(err)
	}

	originalRun, originalActive := runServiceCommand, serviceActive
	t.Cleanup(func() { runServiceCommand, serviceActive = originalRun, originalActive })
	restartedLegacy := false
	runServiceCommand = func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" && len(args) == 2 && args[0] == "load" && filepath.Clean(args[1]) == filepath.Clean(legacyPlist) {
			restartedLegacy = true
		}
		return nil, nil
	}
	newChecks := 0
	serviceActive = func(name string, args ...string) (bool, error) {
		if args[len(args)-1] == legacyLaunchdLabel {
			return false, nil
		}
		newChecks++
		return newChecks > 1, nil
	}
	if err := installLaunchd(false); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("error = %v", err)
	}
	if restartedLegacy {
		t.Fatal("legacy launchd agent restarted while Temper was still running")
	}
	if _, err := os.Stat(filepath.Join(home, migrate.CurrentDirName)); err != nil {
		t.Fatalf("current home was rolled back while Temper was running: %v", err)
	}
}
