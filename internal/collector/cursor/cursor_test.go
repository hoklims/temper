package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Build a fake filesystem layout and verify the dash-ambiguous resolution logic by pointing
// resolveFrom at a temp root instead of "/".
func TestResolveFromHandlesDashedDirNames(t *testing.T) {
	root := t.TempDir()
	// <root>/Users/alex/Documents/Code/YC-Hackathon (dir name contains a dash)
	deep := filepath.Join(root, "Users", "alex", "Documents", "Code", "YC-Hackathon")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got := resolveFrom(root, strings.Split("Users-alex-Documents-Code-YC-Hackathon", "-"))
	if got != deep {
		t.Fatalf("resolveFrom = %q, want %q", got, deep)
	}
}

func TestResolveFromReturnsEmptyWhenMissing(t *testing.T) {
	root := t.TempDir()
	if got := resolveFrom(root, strings.Split("No-Such-Path-Anywhere", "-")); got != "" {
		t.Fatalf("expected no match, got %q", got)
	}
}

func TestParseRealShape(t *testing.T) {
	dir := t.TempDir()
	sdir := filepath.Join(dir, "proj", "agent-transcripts", "abc-123")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"role":"user","message":{"content":[{"type":"text","text":"fix the build"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"On it."},{"type":"text","text":"Done."}]}}
not json at all
{"role":"assistant","message":{"content":[]}}
`
	f := filepath.Join(sdir, "abc-123.jsonl")
	if err := os.WriteFile(f, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New(dir)
	sess, err := a.Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "abc-123" || sess.Tool != "cursor" {
		t.Fatalf("bad identity: %+v", sess)
	}
	if len(sess.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d: %+v", len(sess.Turns), sess.Turns)
	}
	if sess.Turns[1].Text != "On it.\nDone." {
		t.Fatalf("content blocks not joined: %q", sess.Turns[1].Text)
	}
}
