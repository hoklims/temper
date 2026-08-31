// Package migrate moves legacy AutoSkills state into Temper without merging or overwriting data.
package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	CurrentDirName = ".temper"
	LegacyDirName  = ".autoskills"
	CurrentDBName  = "temper.db"
	LegacyDBName   = "autoskills.db"
)

type Result struct {
	Migrated bool
	From     string
	To       string
}

// Plan is a prepared, resumable home migration. Preparation normalizes the database name but
// deliberately does not move the directory, so callers can reconcile persisted absolute legacy
// paths before applying the move.
type Plan struct {
	legacy      string
	current     string
	moveHome    bool
	dbRenamedIn string
}

func CurrentDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return CurrentDirName
	}
	return filepath.Join(home, CurrentDirName)
}

func PrepareHome() (Plan, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Plan{}, fmt.Errorf("resolve user home: %w", err)
	}
	return PrepareHomeAt(home)
}

// PrepareHomeAt validates both homes and renames autoskills.db before any directory move. Thus
// legacy/temper.db is a resumable prepared state, while current/autoskills.db is recognized and
// completed as a partial migration left by an older build.
func PrepareHomeAt(home string) (Plan, error) {
	legacy := filepath.Join(home, LegacyDirName)
	current := filepath.Join(home, CurrentDirName)
	legacyInfo, legacyErr := os.Stat(legacy)
	currentInfo, currentErr := os.Stat(current)

	if legacyErr == nil && currentErr == nil {
		return Plan{}, fmt.Errorf("both %s and %s exist; refusing to merge state automatically", current, legacy)
	}
	if legacyErr != nil && !errors.Is(legacyErr, os.ErrNotExist) {
		return Plan{}, legacyErr
	}
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return Plan{}, currentErr
	}
	if legacyErr == nil && !legacyInfo.IsDir() {
		return Plan{}, fmt.Errorf("legacy home %s is not a directory", legacy)
	}
	if currentErr == nil && !currentInfo.IsDir() {
		return Plan{}, fmt.Errorf("temper home %s is not a directory", current)
	}

	plan := Plan{legacy: legacy, current: current, moveHome: legacyErr == nil}
	dir := current
	if plan.moveHome {
		dir = legacy
	} else if currentErr != nil {
		return plan, nil
	}
	needsMigration := plan.moveHome
	if !needsMigration {
		if _, err := os.Stat(filepath.Join(dir, LegacyDBName)); err == nil {
			needsMigration = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return Plan{}, err
		}
	}
	if needsMigration {
		if err := refuseSQLiteSidecars(dir); err != nil {
			return Plan{}, err
		}
	}
	renamed, err := normalizeDatabase(dir)
	if err != nil {
		return Plan{}, err
	}
	if renamed {
		plan.dbRenamedIn = dir
	} else if plan.moveHome {
		// A previous process may have crashed after normalizing the database but before moving
		// the home. This resumed plan still owns that rename and must undo it if a later service
		// activation rolls the migration back.
		if _, err := os.Stat(filepath.Join(dir, CurrentDBName)); err == nil {
			plan.dbRenamedIn = dir
		} else if !errors.Is(err, os.ErrNotExist) {
			return Plan{}, err
		}
	}
	return plan, nil
}

func refuseSQLiteSidecars(dir string) error {
	for _, name := range []string{LegacyDBName, CurrentDBName} {
		for _, suffix := range []string{"-wal", "-shm"} {
			path := filepath.Join(dir, name+suffix)
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("database sidecar %s exists; stop AutoSkills/Temper and checkpoint the database before migration", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func normalizeDatabase(dir string) (bool, error) {
	oldDB := filepath.Join(dir, LegacyDBName)
	newDB := filepath.Join(dir, CurrentDBName)
	_, oldErr := os.Stat(oldDB)
	_, newErr := os.Stat(newDB)
	if oldErr == nil && newErr == nil {
		return false, fmt.Errorf("home %s contains both %s and %s; refusing to choose one", dir, LegacyDBName, CurrentDBName)
	}
	if oldErr != nil && !errors.Is(oldErr, os.ErrNotExist) {
		return false, oldErr
	}
	if newErr != nil && !errors.Is(newErr, os.ErrNotExist) {
		return false, newErr
	}
	if oldErr == nil {
		if err := os.Rename(oldDB, newDB); err != nil {
			return false, fmt.Errorf("rename legacy database %s to %s: %w", oldDB, newDB, err)
		}
		return true, nil
	}
	return false, nil
}

func (p Plan) ReconcileDir() string {
	if p.moveHome {
		return p.legacy
	}
	return p.current
}

func (p Plan) DatabasePath() string { return filepath.Join(p.ReconcileDir(), CurrentDBName) }

func (p Plan) HasDatabase() (bool, error) {
	_, err := os.Stat(p.DatabasePath())
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (p Plan) Apply() (Result, error) {
	if !p.moveHome {
		return Result{Migrated: p.dbRenamedIn != "", From: p.current, To: p.current}, nil
	}
	if err := os.Rename(p.legacy, p.current); err != nil {
		return Result{}, fmt.Errorf("move legacy home %s to %s: %w", p.legacy, p.current, err)
	}
	return Result{Migrated: true, From: p.legacy, To: p.current}, nil
}

// Rollback restores a migration applied by this plan. It never merges homes or overwrites either
// database name, and reports every failed compensating action.
func (p Plan) Rollback() error {
	var errs []error
	if p.moveHome {
		_, legacyErr := os.Stat(p.legacy)
		_, currentErr := os.Stat(p.current)
		switch {
		case legacyErr == nil && errors.Is(currentErr, os.ErrNotExist):
			// Apply was never reached (or failed before the rename); only the database name may
			// need compensation below.
		case errors.Is(legacyErr, os.ErrNotExist) && currentErr == nil:
			if err := os.Rename(p.current, p.legacy); err != nil {
				errs = append(errs, fmt.Errorf("restore legacy home: %w", err))
			}
		case legacyErr == nil && currentErr == nil:
			errs = append(errs, fmt.Errorf("cannot rollback: both %s and %s exist", p.legacy, p.current))
		case !errors.Is(legacyErr, os.ErrNotExist):
			errs = append(errs, legacyErr)
		case !errors.Is(currentErr, os.ErrNotExist):
			errs = append(errs, currentErr)
		}
	}
	if p.dbRenamedIn != "" {
		dir := p.dbRenamedIn
		if p.moveHome {
			dir = p.legacy
		}
		oldDB, newDB := filepath.Join(dir, LegacyDBName), filepath.Join(dir, CurrentDBName)
		if _, err := os.Stat(oldDB); err == nil {
			errs = append(errs, fmt.Errorf("cannot rollback database rename: %s already exists", oldDB))
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		} else if _, err := os.Stat(newDB); err == nil {
			if err := os.Rename(newDB, oldDB); err != nil {
				errs = append(errs, fmt.Errorf("restore legacy database name: %w", err))
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func EnsureHome() (Result, error) {
	plan, err := PrepareHome()
	if err != nil {
		return Result{}, err
	}
	return plan.Apply()
}

func EnsureHomeAt(home string) (Result, error) {
	plan, err := PrepareHomeAt(home)
	if err != nil {
		return Result{}, err
	}
	result, err := plan.Apply()
	if err == nil {
		return result, nil
	}
	if rollbackErr := plan.Rollback(); rollbackErr != nil {
		return Result{}, fmt.Errorf("%w; rollback failed: %v", err, rollbackErr)
	}
	return Result{}, err
}
