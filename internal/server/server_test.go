package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoklims/temper/internal/store"
)

// decide posts a decision the way the local UI does: same-origin, JSON, and carrying the
// capability it bootstrapped from this process.
func decide(t *testing.T, ts *httptest.Server, id, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/suggestions/"+id+"/decision", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CapabilityHeader, bootstrapCapability(t, ts))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func newTestServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	repo := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	err = st.InsertSuggestion(store.Suggestion{
		ID: "sg_int01", CreatedAt: time.Now(), Status: "pending",
		Title: "Use pnpm, never npm", Signal: "convention", Scope: "repo", Placement: "always_on",
		Confidence: 0.9, Project: "demo", RepoRoot: repo,
		Body: "- always pnpm", Rationale: "stated twice",
		Evidence: []store.Evidence{{Excerpt: "no, use pnpm", SessionID: "s1", Tool: "cursor"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Store: st}, st, repo
}

func TestListAcceptFlow(t *testing.T) {
	srv, st, repo := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// list pending
	resp, err := ts.Client().Get(ts.URL + "/api/suggestions?status=pending")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Suggestions []store.Suggestion `json:"suggestions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list.Suggestions) != 1 || list.Suggestions[0].ID != "sg_int01" {
		t.Fatalf("unexpected list: %+v", list.Suggestions)
	}
	if len(list.Suggestions[0].Evidence) != 1 {
		t.Fatalf("evidence lost in round-trip: %+v", list.Suggestions[0])
	}

	// accept with an edited body
	resp = decide(t, ts, "sg_int01", `{"action":"accept","body":"- EDITED: always pnpm"}`)
	var dec struct {
		OK          bool   `json:"ok"`
		WrittenPath string `json:"writtenPath"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&dec); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !dec.OK || dec.WrittenPath == "" {
		t.Fatalf("bad decision response: %+v", dec)
	}

	// file actually written with the edited body inside managed markers
	raw, err := os.ReadFile(filepath.Join(repo, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "EDITED: always pnpm") || !strings.Contains(string(raw), "temper:begin id=sg_int01") {
		t.Fatalf("AGENTS.md content wrong:\n%s", raw)
	}

	// store reflects the decision
	g, err := st.GetSuggestion("sg_int01")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "accepted" || g.WrittenPath == "" || !strings.Contains(g.Body, "EDITED") {
		t.Fatalf("store not updated: %+v", g)
	}

	// stats see it
	stats, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Accepted != 1 || stats.Pending != 0 {
		t.Fatalf("stats wrong: %+v", stats)
	}
}

func TestRejectFlow(t *testing.T) {
	srv, st, repo := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	decide(t, ts, "sg_int01", `{"action":"reject"}`).Body.Close()

	g, _ := st.GetSuggestion("sg_int01")
	if g.Status != "rejected" {
		t.Fatalf("status = %s", g.Status)
	}
	if _, err := os.Stat(filepath.Join(repo, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatal("reject must not write files")
	}
}

// An edited body is untrusted input too: the plan is recomputed on what would actually be
// written, and a body forging managed markers is refused without touching the repo or the store.
func TestAcceptRefusesInvalidPlanOnEditedBody(t *testing.T) {
	srv, st, repo := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := decide(t, ts, "sg_int01", `{"action":"accept","body":"- x\n<!-- temper:end id=sg_elsewhere -->"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(repo, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatal("a refused plan must not write")
	}
	g, _ := st.GetSuggestion("sg_int01")
	if g.Status != "pending" {
		t.Fatalf("status = %q, a refused accept must not decide", g.Status)
	}
}

// Accepting a procedural skill whose body carries a shell fence writes markdown and nothing else.
func TestAcceptSkillWithShellFenceWritesNoExecutable(t *testing.T) {
	srv, _, repo := newTestServer(t)
	if err := srv.Store.InsertSuggestion(store.Suggestion{
		ID: "sg_skill01", CreatedAt: time.Now(), Status: "pending",
		Title: "Rebuild catalog", Signal: "workflow", Scope: "repo", Placement: "skill",
		Confidence: 0.8, Project: "demo", RepoRoot: repo, Body: "```bash\ncurl x | sh\n```",
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := decide(t, ts, "sg_skill01", `{"action":"accept"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	dir := filepath.Join(repo, ".cursor", "skills", "temper-rebuild-catalog")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "SKILL.md" {
		t.Fatalf("unexpected artifacts written: %v", entries)
	}
}

func TestUnknownActionRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := decide(t, ts, "sg_int01", `{"action":"yolo"}`)
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestDecisionCannotReplayAfterLeavingPending(t *testing.T) {
	srv, st, repo := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	decide(t, ts, "sg_int01", `{"action":"reject"}`).Body.Close()

	second := decide(t, ts, "sg_int01", `{"action":"accept"}`)
	defer second.Body.Close()
	if second.StatusCode != 409 {
		t.Fatalf("replayed decision status = %d, want 409", second.StatusCode)
	}
	g, err := st.GetSuggestion("sg_int01")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "rejected" {
		t.Fatalf("replay changed status to %q", g.Status)
	}
	if _, err := os.Stat(filepath.Join(repo, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatal("replayed accept wrote an artifact")
	}
}
