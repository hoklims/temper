package writer

import (
	"os"
	"path/filepath"
	"testing"
)

// Temper deletes exactly one temporary file: the one the current write created, by its exact
// name. It does not collect by prefix, and these tests are why.
//
// A prefix is not a proof of ownership. It matches a file a user happened to name that way, and it
// matches the live temporary of another mutation that is in flight in the same directory — a file
// whose whole purpose is to be renamed into place a moment later. An orphan left by a process that
// died is untidy and harmless; a sweep that removes either of those two is neither.

// A file the user owns, sitting in the directory a mutation is about to write in, and named the
// way temper names its own temporaries. Nothing in the manifest mentions it, so nothing may
// remove it.
func TestAMutationLeavesAUserFileThatLooksLikeATemporary(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(target, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(root, tempPrefix+"not-ours"+tempSuffix)
	if err := os.WriteFile(userFile, []byte("user content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mut := Mutation{Ops: []FileOp{{Root: root, Path: target, Content: "after\n"}}, WrittenPath: target}
	if err := capture(&mut); err != nil {
		t.Fatal(err)
	}
	if err := applyOps(&mut); err != nil {
		t.Fatal(err)
	}
	if got := read(t, userFile); got != "user content\n" {
		t.Fatalf("a mutation deleted a file outside its manifest: %q", got)
	}
	if got := read(t, target); got != "after\n" {
		t.Fatalf("the mutation did not do its own work: %q", got)
	}
	// and the rollback path is a deletion path too
	if err := unwind(&mut); err != nil {
		t.Fatal(err)
	}
	if got := read(t, userFile); got != "user content\n" {
		t.Fatalf("a rollback deleted a file outside its manifest: %q", got)
	}
}

// Two mutations on two different files in one directory are allowed to run at the same time: they
// claim different resources, so nothing serializes them. This is the exact reachable state of the
// one that is mid-write — its temporary exists and has not been renamed yet — and the other one
// must be incapable of removing it, in either direction.
func TestTwoDistinctMutationsCannotDeleteEachOthersLiveTemporaries(t *testing.T) {
	for _, direction := range []struct{ name, runs, waits string }{
		{"second mutation runs while the first holds a temporary", "second.md", "first.md"},
		{"first mutation runs while the second holds a temporary", "first.md", "second.md"},
	} {
		t.Run(direction.name, func(t *testing.T) {
			root := t.TempDir()
			running := filepath.Join(root, direction.runs)
			waiting := filepath.Join(root, direction.waits)
			if err := os.WriteFile(running, []byte("running old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(waiting, []byte("waiting old\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			mut := Mutation{Ops: []FileOp{{Root: root, Path: running, Content: "running new\n"}}, WrittenPath: running}
			if err := capture(&mut); err != nil {
				t.Fatal(err)
			}

			// the other mutation is between "temporary fully written" and "renamed into place"
			liveTemp := filepath.Join(root, tempName())
			if err := os.WriteFile(liveTemp, []byte("waiting new\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			if err := applyOps(&mut); err != nil {
				t.Fatal(err)
			}
			if got := read(t, liveTemp); got != "waiting new\n" {
				t.Fatalf("one mutation removed another's live temporary: %q", got)
			}
			// which is the property that matters: the other mutation's atomic rename still works
			if err := os.Rename(liveTemp, waiting); err != nil {
				t.Fatalf("the other mutation could no longer complete its atomic write: %v", err)
			}
			if got := read(t, waiting); got != "waiting new\n" {
				t.Fatalf("the other mutation's result is wrong: %q", got)
			}
			if got := read(t, running); got != "running new\n" {
				t.Fatalf("the mutation that ran did not do its own work: %q", got)
			}
			// the mutation that ran left no temporary of its own behind
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != filepath.Base(running) && e.Name() != filepath.Base(waiting) {
					t.Fatalf("the completed mutation left %s behind", e.Name())
				}
			}
		})
	}
}
