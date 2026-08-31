package distill

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hoklims/temper/internal/cache"
	"github.com/hoklims/temper/internal/canon"
	"github.com/hoklims/temper/internal/llm"
)

func TestSeenContentSkipsDuplicateLLMCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"suggestions\":[]}"}}]}`))
	}))
	defer srv.Close()

	client, err := llm.New(srv.URL, "", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	d := &Distiller{
		Provider:    client,
		SeenContent: cache.New[string, bool](16),
	}
	s := &canon.Session{
		ID:    "s1",
		Tool:  "cursor",
		Turns: []canon.Turn{{Role: canon.RoleUser, Text: "we use pnpm not npm"}, {Role: canon.RoleAssistant, Text: "ok"}},
	}

	if _, err := d.Session(context.Background(), s); err != nil {
		t.Fatalf("first Session: %v", err)
	}
	if _, err := d.Session(context.Background(), s); err != nil {
		t.Fatalf("second Session: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("LLM was called %d times; want 1 (second identical call should hit the seen-content cache)", got)
	}
}
