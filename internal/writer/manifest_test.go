package writer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoklims/temper/internal/store"
)

// A manifest is a row in a database. It is the only thing that says which bytes may be written and
// through which directory, and one of the paths it drives deletes files — so the format is closed:
// what this build did not write, this build does not act on.
//
// The asymmetry below is deliberate and is the point of these tests. A PREPARED operation is proof
// that no byte moved, so releasing it is safe without reading its manifest at all; an unreadable
// entry must not become a permanent blockage. An APPLYING or ROLLING_BACK one may have written, so
// a manifest that cannot be read is a manifest that cannot say what to finish or what to restore —
// and the honest answer is to touch nothing and keep the files reserved.

// plantOperation puts an operation with an arbitrary manifest into the journal, in the state a
// crash would have left it, holding a real claim on the file it names.
func plantOperation(t *testing.T, st *store.Store, g store.Suggestion, state, manifest, target string) string {
	t.Helper()
	resource, err := resourceKey(target)
	if err != nil {
		t.Fatal(err)
	}
	op := store.Operation{
		ID: store.NewOperationID(), SuggestionID: g.ID, Kind: "accept", Manifest: manifest,
		TargetStatus: "accepted", TargetBody: g.Body, TargetPath: target,
	}
	if err := st.BeginOperation(op, "pending", []string{resource}); err != nil {
		t.Fatal(err)
	}
	switch state {
	case store.OpPrepared:
	case store.OpApplying:
		if err := st.MarkApplying(op.ID); err != nil {
			t.Fatal(err)
		}
	case store.OpRollingBack:
		if err := st.MarkApplying(op.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkRollingBack(op.ID, "planted mid-restoration"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported planted state %q", state)
	}
	return op.ID
}

// legacyManifest is the shape builds before this one wrote: no version, the authority over a root
// recorded as a pathname, and a list of directories to prune. Every one of those is a decision
// this build makes differently, which is exactly why it must not be read as if it agreed.
func legacyManifest(repo string) string {
	root := strings.ReplaceAll(filepath.Clean(repo), `\`, `\\`)
	agents := strings.ReplaceAll(filepath.Join(repo, "AGENTS.md"), `\`, `\\`)
	return `{"ops":[{"root":"` + root + `","path":"` + agents +
		`","content":"legacy\n","existed":true,"preimage":"before\n","preimageSum":"x","postSum":"y","mode":420,"postMode":420}],` +
		`"pruneDirs":[],"rootIds":{"` + root + `":"` + root + `"},"writtenPath":"` + agents + `"}`
}

func manifestFor(t *testing.T, repo, shape string) string {
	t.Helper()
	if shape == "legacy" {
		return legacyManifest(repo)
	}
	return shape
}

// A prepared operation is released on its own terms: the state is the proof, not the bytes.
// Nothing was written, so nothing has to be understood to let go of it.
func TestPreparedOperationIsReleasedWithoutReadingItsManifest(t *testing.T) {
	for _, tc := range []struct{ name, shape string }{
		{"empty object", `{}`},
		{"not json at all", `this was never a manifest`},
		{"legacy shape", "legacy"},
		{"unknown version", `{"version":99,"ops":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, repo, g := journalRepo(t)
			before := snapshotTree(t, repo)
			agents := filepath.Join(repo, "AGENTS.md")
			opID := plantOperation(t, st, g, store.OpPrepared, manifestFor(t, repo, tc.shape), agents)

			report, err := Reconcile(st)
			if err != nil {
				t.Fatalf("releasing an operation that provably wrote nothing needed its manifest: %v", err)
			}
			if len(report) != 1 || !strings.Contains(report[0], "nothing had been written") {
				t.Fatalf("reconcile report = %v", report)
			}
			assertSameTree(t, "released prepared operation", before, snapshotTree(t, repo))
			if open, err := st.IncompleteOperations(); err != nil || len(open) != 0 {
				t.Fatalf("the released operation kept its claims: %+v (%v)", open, err)
			}
			stored, err := st.GetOperation(opID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != store.OpRolledBack {
				t.Fatalf("a released operation is %s", stored.State)
			}
			// and the file it named is genuinely free again
			if _, err := Accept(st, g); err != nil {
				t.Fatalf("the released claim still blocks a fresh decision: %v", err)
			}
		})
	}
}

// The other side of the asymmetry. An operation that may have written cannot be finished, restored
// or released on a manifest this build does not understand: it knows neither what the postimage
// was meant to be nor what preimage to put back. It says so, touches nothing, and keeps the files
// reserved for a human.
func TestApplyingAndRollingBackRefuseAManifestThisBuildDoesNotWrite(t *testing.T) {
	shapes := []struct{ name, shape string }{
		{"empty object", `{}`},
		{"legacy shape", "legacy"},
		{"unknown version", `{"version":99,"ops":[{"root":"/r","path":"/r/a.md"}]}`},
		{"unknown field on the current version", `{"version":1,"ops":[],"pruneDirs":[]}`},
		{"trailing json", `{"version":1,"ops":[]} {"version":1,"ops":[]}`},
		{"unreadable", `{"version":1,"ops":`},
	}
	for _, state := range []string{store.OpApplying, store.OpRollingBack} {
		for _, tc := range shapes {
			t.Run(state+" "+tc.name, func(t *testing.T) {
				st, repo, g := journalRepo(t)
				before := snapshotTree(t, repo)
				agents := filepath.Join(repo, "AGENTS.md")
				opID := plantOperation(t, st, g, state, manifestFor(t, repo, tc.shape), agents)

				report, err := Reconcile(st)
				if err == nil {
					t.Fatalf("an operation in %s was reconciled from a manifest this build does not write: %v", state, report)
				}
				if !strings.Contains(err.Error(), opID) {
					t.Fatalf("the refusal must name the operation a human has to resolve: %v", err)
				}
				assertSameTree(t, "refused "+state, before, snapshotTree(t, repo))
				open, oErr := st.IncompleteOperations()
				if oErr != nil {
					t.Fatal(oErr)
				}
				if len(open) != 1 || open[0].ID != opID || open[0].State != state {
					t.Fatalf("a refused reconciliation must leave the operation exactly as it was, got %+v", open)
				}
				// the claims are still held, so nothing else may touch the contested file
				second := g
				second.ID = "sg_manifest_contender"
				second.Title = "Second rule aimed at the same file"
				second.Body = "- second body"
				if err := st.InsertSuggestion(second); err != nil {
					t.Fatal(err)
				}
				if _, err := Accept(st, second); err == nil {
					t.Fatal("a file reserved by an unresolvable operation was handed to another acceptance")
				}
			})
		}
	}
}

// Version alone is not the format. A manifest can carry the current version and still describe a
// state no capture could have produced — a checksum that does not match the bytes beside it, a
// root nothing proves, an authority for a root no operation uses. Each of those decides what gets
// written or deleted, so each is refused before it can.
func TestCurrentVersionManifestWithAnImpossibleStructureIsRefused(t *testing.T) {
	repo := t.TempDir()
	agents := filepath.Join(repo, "AGENTS.md")
	if err := os.WriteFile(agents, []byte("# AGENTS.md\n\nHand-written intro.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	valid, err := BuildMutation(suggestion(repo))
	if err != nil {
		t.Fatal(err)
	}
	if err := capture(&valid); err != nil {
		t.Fatal(err)
	}
	if len(valid.Ops) != 1 || !valid.Ops[0].Existed {
		t.Fatalf("setup wrong: expected one replacement of an existing file, got %+v", valid.Ops)
	}

	for _, tc := range []struct {
		name    string
		corrupt func(m *Mutation)
		mustSay string
	}{
		{"postimage checksum does not match the content", func(m *Mutation) {
			m.Ops[0].PostSum = strings.Repeat("0", 64)
		}, "postimage checksum"},
		{"preimage checksum does not match the preimage", func(m *Mutation) {
			m.Ops[0].Preimage = "something else entirely"
		}, "preimage checksum"},
		{"a file that did not exist also has a preimage", func(m *Mutation) {
			m.Ops[0].Existed = false
		}, "did not exist and also describes"},
		{"a removal that also writes content", func(m *Mutation) {
			m.Ops[0].Remove = true
		}, "removes the file and also describes"},
		{"a replacement with no postimage mode", func(m *Mutation) {
			m.Ops[0].PostModeKnown = false
		}, "records no permissions"},
		{"no authority for a root it writes through", func(m *Mutation) {
			m.Roots = map[string]RootAuthority{}
		}, "records no authority over it"},
		{"an authority no operation uses", func(m *Mutation) {
			spare := filepath.Join(filepath.Dir(filepath.Clean(repo)), "unused-root")
			m.Roots[spare] = RootAuthority{
				Root: spare, Anchor: filepath.Dir(spare), AnchorID: "unix:1:1", Suffix: "unused-root",
			}
		}, "none of its operations uses"},
		{"an authority with no identity for its ancestor", func(m *Mutation) {
			auth := m.Roots[filepath.Clean(repo)]
			auth.AnchorID, auth.RootID = "", ""
			m.Roots[filepath.Clean(repo)] = auth
		}, "records no identity for its ancestor"},
		{"an authority whose suffix leaves its ancestor", func(m *Mutation) {
			auth := m.Roots[filepath.Clean(repo)]
			auth.Anchor, auth.Suffix = filepath.Dir(filepath.Clean(repo)), ".."
			m.Roots[filepath.Clean(repo)] = auth
		}, "not confined to its ancestor"},
		{"an authority that leads somewhere else", func(m *Mutation) {
			auth := m.Roots[filepath.Clean(repo)]
			auth.Anchor, auth.Suffix = filepath.Dir(filepath.Clean(repo)), "elsewhere"
			m.Roots[filepath.Clean(repo)] = auth
		}, "leads to"},
		{"a root that is its own ancestor with a different identity", func(m *Mutation) {
			auth := m.Roots[filepath.Clean(repo)]
			auth.RootID = "unix:9:9"
			m.Roots[filepath.Clean(repo)] = auth
		}, "records a different identity"},
		{"no operation at all", func(m *Mutation) { m.Ops = nil }, "carries no operation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := cloneMutation(t, valid)
			tc.corrupt(&broken)
			raw, err := json.Marshal(broken)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeManifest(string(raw)); err == nil {
				t.Fatal("a manifest describing a state no capture could produce was accepted")
			} else if !strings.Contains(err.Error(), tc.mustSay) {
				t.Fatalf("the refusal does not name what is wrong (want %q): %v", tc.mustSay, err)
			}
		})
	}

	// the control: untouched, the same manifest round-trips and is accepted
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeManifest(string(raw)); err != nil {
		t.Fatalf("a manifest this build wrote was refused by its own decoder: %v", err)
	}
}

func cloneMutation(t *testing.T, m Mutation) Mutation {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out Mutation
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
