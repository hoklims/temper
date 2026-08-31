package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoklims/temper/internal/config"
	"github.com/hoklims/temper/internal/llm"
	"github.com/hoklims/temper/internal/store"
)

func TestCommandUsesStateExcludesServiceManagement(t *testing.T) {
	for _, command := range []string{"scan", "review", "status", "daemon", "garden", "verify", "undo"} {
		if !commandUsesState(command) {
			t.Fatalf("%s should migrate state before running", command)
		}
	}
	for _, command := range []string{"install-daemon", "version", "help"} {
		if commandUsesState(command) {
			t.Fatalf("%s should remain usable when state migration is blocked", command)
		}
	}
}

func TestConfiguredProviderSelection(t *testing.T) {
	if _, err := configuredProvider(config.Config{Provider: "http", Endpoint: "http://localhost:11434/v1"}); err != nil {
		t.Fatalf("keyless loopback HTTP provider: %v", err)
	}
	if _, err := configuredProvider(config.Config{Provider: "unknown"}); err == nil {
		t.Fatal("unknown provider must fail")
	}
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"codex", "claude"} {
		if _, err := configuredProvider(config.Config{Provider: name}); !errors.Is(err, llm.ErrCLIUnavailable) {
			t.Fatalf("%s missing CLI error = %v", name, err)
		}
	}
}

func TestCodexScanSmoke(t *testing.T) {
	if os.Getenv("TEMPER_CODEX_SCAN_SMOKE") == "" {
		t.Skip("set TEMPER_CODEX_SCAN_SMOKE=1 to run a scan with the authenticated Codex CLI")
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)
	plantClaudeTranscript(t, home, repo)
	storage, err := store.Open(filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cfg := config.Config{Provider: "codex", MaxSuggestionsPerScan: 10, MinConfidence: 0.5, SectionBudgetBytes: 12000}
	if err := runScan(ctx, cfg, storage, scanOptions{since: time.Hour, maxSessions: 1}); err != nil {
		t.Fatal(err)
	}
}

const evidenceLine = "we always use pnpm in this repo, never npm — the preinstall hook redirects"

// plantClaudeTranscript writes a transcript rich enough to pass worthDistilling and HasSignal.
func plantClaudeTranscript(t *testing.T, home, repo string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	padding := strings.Repeat("the agent explained the repository layout in detail. ", 30)
	lines := []map[string]any{
		{"type": "user", "cwd": repo, "timestamp": time.Now().Format(time.RFC3339),
			"message": map[string]any{"role": "user", "content": evidenceLine}},
		{"type": "assistant",
			"message": map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": padding}}}},
		{"type": "user", "cwd": repo,
			"message": map[string]any{"role": "user", "content": "no, don't run npm install — " + padding}},
	}
	var sb strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(raw)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "sess1.jsonl"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// captureStderr swaps os.Stderr for the duration of fn and returns what was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stderr.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = f
	fn()
	os.Stderr = orig
	f.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestScanNeverWritesEvenWithLegacyAutoAcceptThreshold is the end-to-end refutation of the
// removed auto-accept tier: a high-confidence, non-sensitive suggestion, a configured legacy
// threshold below it, and still no file and no accepted status anywhere.
func TestScanNeverWritesEvenWithLegacyAutoAcceptThreshold(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	plantClaudeTranscript(t, home, repo)

	reply := `{"suggestions":[{"title":"Use pnpm, never npm","signal":"convention","scope":"repo",` +
		`"placement":"always_on","sensitivity":false,"confidence":0.99,"body":"- always ` + "`pnpm install`" + `",` +
		`"rationale":"stated by the user","evidence":["` + evidenceLine + `"]}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		out, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Config{
		Endpoint: srv.URL, APIKey: "sk-test", Model: "test-model",
		MaxSuggestionsPerScan: 10, MinConfidence: 0.5,
		AutoAcceptThreshold: 0.5, // legacy config that used to write straight to disk
		SectionBudgetBytes:  12000,
	}

	var scanErr error
	stderr := captureStderr(t, func() {
		scanErr = runScan(context.Background(), cfg, st, scanOptions{since: time.Hour, maxSessions: 5})
	})
	if scanErr != nil {
		t.Fatal(scanErr)
	}

	if !strings.Contains(stderr, "auto_accept_threshold") || !strings.Contains(stderr, "IGNORED") {
		t.Fatalf("operator was not warned about the ignored legacy threshold:\n%s", stderr)
	}

	stats, err := st.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 1 || stats.Accepted != 0 {
		t.Fatalf("expected 1 pending / 0 accepted, got %+v", stats)
	}
	pending, err := st.ListSuggestions("pending")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].WrittenPath != "" {
		t.Fatalf("suggestion should be pending with no artifact: %+v", pending)
	}

	// the two places an auto-accept used to land
	if _, err := os.Stat(filepath.Join(repo, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("scan wrote AGENTS.md (err=%v)", err)
	}
	if entries, err := os.ReadDir(filepath.Join(home, ".temper", "skills")); err == nil && len(entries) > 0 {
		t.Fatalf("scan wrote machine skills: %v", entries)
	}
	if entries, err := os.ReadDir(repo); err != nil || len(entries) != 0 {
		t.Fatalf("scan touched the repo: %v (err=%v)", entries, err)
	}
}

// TestScanRefusesUnvettedEndpointBeforeAnyCall proves the endpoint policy gates the whole scan,
// not just the client constructor: a remote plaintext endpoint aborts before a transcript is read.
func TestScanRefusesUnvettedEndpoint(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	plantClaudeTranscript(t, home, repo)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Config{
		Endpoint: "http://api.example.com/v1", APIKey: "sk-test", Model: "m",
		MaxSuggestionsPerScan: 10, MinConfidence: 0.5, SectionBudgetBytes: 12000,
	}
	err = runScan(context.Background(), cfg, st, scanOptions{since: time.Hour, maxSessions: 5})
	if err == nil {
		t.Fatal("scan must refuse a plaintext remote endpoint")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Fatalf("error should explain the TLS requirement: %v", err)
	}
}
