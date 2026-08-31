package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureHomeAtMigratesLegacyDirectoryAndDatabase(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, LegacyDirName)
	if err := os.MkdirAll(filepath.Join(legacy, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, LegacyDBName), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := EnsureHomeAt(home)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated || result.From != legacy || result.To != filepath.Join(home, CurrentDirName) {
		t.Fatalf("unexpected result: %+v", result)
	}
	for _, path := range []string{
		filepath.Join(result.To, CurrentDBName),
		filepath.Join(result.To, "config.json"),
		filepath.Join(result.To, "skills"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("migrated path %s: %v", path, err)
		}
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy directory remains: %v", err)
	}
}

func TestEnsureHomeAtRefusesConflictingHomes(t *testing.T) {
	home := t.TempDir()
	for _, name := range []string{LegacyDirName, CurrentDirName} {
		if err := os.Mkdir(filepath.Join(home, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := EnsureHomeAt(home); err == nil || !strings.Contains(err.Error(), "refusing to merge") {
		t.Fatalf("error = %v", err)
	}
}

func TestEnsureHomeAtRefusesLiveSQLiteSidecars(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, LegacyDirName)
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, LegacyDBName+"-wal"), []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureHomeAt(home); err == nil || !strings.Contains(err.Error(), "stop AutoSkills") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy home moved despite refusal: %v", err)
	}
}

func TestEnsureHomeAtIsIdempotent(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, CurrentDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := EnsureHomeAt(home)
	if err != nil || result.Migrated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPrepareHomeAtLeavesCurrentDatabaseSidecarsToSQLiteRecovery(t *testing.T) {
	home := t.TempDir()
	current := filepath.Join(home, CurrentDirName)
	if err := os.Mkdir(current, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{CurrentDBName, CurrentDBName + "-wal", CurrentDBName + "-shm"} {
		if err := os.WriteFile(filepath.Join(current, name), []byte("state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := PrepareHomeAt(home)
	if err != nil {
		t.Fatalf("ordinary current-home startup was treated as a migration: %v", err)
	}
	if result, err := plan.Apply(); err != nil || result.Migrated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPrepareHomeAtNormalizesDatabaseBeforeMovingHome(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, LegacyDirName)
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, LegacyDBName), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareHomeAt(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("home moved during preparation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, CurrentDBName)); err != nil {
		t.Fatalf("database was not normalized before home move: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, LegacyDBName)); !os.IsNotExist(err) {
		t.Fatalf("legacy database name remains: %v", err)
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureHomeAtResumesCurrentOnlyLegacyDatabaseName(t *testing.T) {
	home := t.TempDir()
	current := filepath.Join(home, CurrentDirName)
	if err := os.Mkdir(current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, LegacyDBName), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := EnsureHomeAt(home)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated || result.From != current || result.To != current {
		t.Fatalf("unexpected partial-migration result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(current, CurrentDBName)); err != nil {
		t.Fatalf("partial database rename was not completed: %v", err)
	}
}

func TestPrepareHomeAtRefusesDatabaseNameConflict(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, LegacyDirName)
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{LegacyDBName, CurrentDBName} {
		if err := os.WriteFile(filepath.Join(legacy, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := PrepareHomeAt(home); err == nil || !strings.Contains(err.Error(), "refusing to choose") {
		t.Fatalf("error = %v", err)
	}
}

func TestPlanRollbackRestoresLegacyNames(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, LegacyDirName)
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, LegacyDBName), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareHomeAt(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(legacy, LegacyDBName)); err != nil {
		t.Fatalf("legacy state not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, CurrentDirName)); !os.IsNotExist(err) {
		t.Fatalf("current home remains after rollback: %v", err)
	}
}

func TestResumedPlanRollbackRestoresLegacyDatabaseName(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, LegacyDirName)
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, CurrentDBName), []byte("prepared before crash"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareHomeAt(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := plan.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(legacy, LegacyDBName)); err != nil {
		t.Fatalf("resumed rollback did not restore legacy database name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, CurrentDBName)); !os.IsNotExist(err) {
		t.Fatalf("Temper database name remains after resumed rollback: %v", err)
	}
}
