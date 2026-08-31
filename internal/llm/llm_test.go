package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hoklims/temper/internal/outbound"
)

func TestValidateEndpointPolicy(t *testing.T) {
	ok := []string{
		"https://api.anthropic.com/v1",
		"https://gateway.corp.example.com:8443/openai/v1",
		"http://localhost:11434/v1",
		"http://127.0.0.1:11434/v1",
		"http://[::1]:11434/v1",
	}
	for _, e := range ok {
		if err := ValidateEndpoint(e); err != nil {
			t.Errorf("endpoint %q must be accepted: %v", e, err)
		}
	}

	bad := []string{
		"http://api.example.com/v1",            // plaintext to a remote host
		"http://10.0.0.5:8080/v1",              // private, but still not loopback
		"https://user:hunter2@api.example.com", // credentials smuggled in the URL
		"http://127.0.0.1@evil.example.com/v1", // loopback-looking userinfo, remote host
		"ftp://api.example.com/v1",
		"file:///tmp/whatever",
		"https://",
		"://nonsense",
		"",
		"https://api.example.com/v1#fragment",
		"https://gateway.example.com/v1?key=secret",   // appending a path would land inside the query
		"https://gateway.example.com/v1?",             // empty query marker is still a query
		"http://localhost:11434/v1?api-version=2024",  // loopback does not make it unambiguous
		"https://gateway.example.com/v1?a=1&b=2#frag", // query and fragment together
	}
	for _, e := range bad {
		if err := ValidateEndpoint(e); err == nil {
			t.Errorf("endpoint %q must be rejected", e)
		}
	}
}

// The refusal has to happen before a client exists, not at request time: the point is that no
// request carrying the key is ever built against an ambiguous destination.
func TestNewRejectsEndpointCarryingQuery(t *testing.T) {
	for _, endpoint := range []string{
		"https://gateway.example.com/v1?key=sk-secret",
		"https://gateway.example.com/v1?",
	} {
		c, err := New(endpoint, "sk-secret", "m")
		if err == nil {
			t.Fatalf("endpoint %q accepted: %+v", endpoint, c)
		}
		if strings.Contains(err.Error(), "sk-secret") {
			t.Fatalf("error echoed the URL credential: %v", err)
		}
	}
	// a clean custom gateway path still works — the rule is about ambiguity, not about custom hosts
	if _, err := New("https://gateway.corp.example.com:8443/openai/v1", "sk-secret", "m"); err != nil {
		t.Fatalf("clean custom gateway refused: %v", err)
	}
}

// A Client built as a struct literal must not be able to put the key on the wire toward an
// ambiguous destination either: the same gate is re-checked before the request is composed.
func TestGenerateRejectsEndpointCarryingQuery(t *testing.T) {
	var reached bool
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer upstream.Close()

	c := &Client{Endpoint: upstream.URL + "/v1?key=secret", APIKey: "sk-secret", Model: "m", HTTP: upstream.Client()}
	if _, err := c.Generate(context.Background(), preparedPayload(t)); err == nil {
		t.Fatal("Generate accepted an endpoint carrying a query")
	}
	if reached {
		t.Fatal("a request was sent to an endpoint that never passed the policy")
	}
}

func TestNewRejectsUnvettedEndpoint(t *testing.T) {
	if _, err := New("http://api.example.com/v1", "sk-secret", "m"); err == nil {
		t.Fatal("New must refuse a plaintext remote endpoint")
	}
	c, err := New("https://api.anthropic.com/v1/", "sk-secret", "m")
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "https://api.anthropic.com/v1" {
		t.Fatalf("endpoint not normalized: %q", c.Endpoint)
	}
}

func TestNewRequiresAPIKeyOnlyForRemoteHTTP(t *testing.T) {
	if _, err := New("https://api.example.com/v1", "", "m"); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("remote keyless provider error = %v", err)
	}
	if _, err := New("http://127.0.0.1:11434/v1", "", "m"); err != nil {
		t.Fatalf("loopback keyless provider error = %v", err)
	}
}

type recordingTransport struct {
	calls int
}

func (rt *recordingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	rt.calls++
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"{}"}}]}`)),
		Header:     http.Header{},
	}, nil
}

func TestChatRefusesUnvettedEndpointBeforeAnyRequest(t *testing.T) {
	rt := &recordingTransport{}
	// built as a struct literal on purpose: the guard must live on the request path, not only in New
	c := &Client{Endpoint: "http://api.example.com/v1", APIKey: "sk-secret", Model: "m", HTTP: &http.Client{Transport: rt}}

	var b outbound.Builder
	b.Static("hello")
	p, err := b.Build("system")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Generate(context.Background(), p); err == nil {
		t.Fatal("Chat must refuse an unvetted endpoint")
	}
	if rt.calls != 0 {
		t.Fatalf("a request carrying the API key was issued anyway (%d calls)", rt.calls)
	}
}

// TestChatBodyCarriesOnlyThePayload inspects the exact bytes on the wire.
func TestChatBodyCarriesOnlyThePayload(t *testing.T) {
	var gotBody []byte
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c, err := New(srv.URL, "sk-secret", "test-model") // httptest binds loopback: http is allowed
	if err != nil {
		t.Fatal(err)
	}
	var b outbound.Builder
	b.Static("PROJECT: ").Data("demo", 100)
	b.Static("\nkey: ").Data("sk-ant-api03-cccccccccccccccccccccccc", 0)
	p, err := b.Build("system prompt")
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.Generate(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok" {
		t.Fatalf("unexpected content %q", out)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if strings.Contains(string(gotBody), "sk-ant-api03-cccccccccccccccccccccccc") {
		t.Fatalf("secret reached the request body:\n%s", gotBody)
	}
	var sent chatRequest
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatal(err)
	}
	if len(sent.Messages) != 2 || sent.Messages[0].Content != p.System() || sent.Messages[1].Content != p.User() {
		t.Fatalf("body is not the prepared payload: %+v", sent.Messages)
	}
}

func TestChatDoesNotFollowRedirects(t *testing.T) {
	unsafeCalls := 0
	unsafeAuth := ""
	unsafe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unsafeCalls++
		unsafeAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer unsafe.Close()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, unsafe.URL, http.StatusTemporaryRedirect)
	}))
	defer entry.Close()

	c, err := New(entry.URL, "sk-secret", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	var b outbound.Builder
	b.Static("hello")
	p, err := b.Build("system")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Generate(context.Background(), p); err == nil {
		t.Fatal("redirected provider response must be refused")
	}
	if unsafeCalls != 0 || unsafeAuth != "" {
		t.Fatalf("redirect target received %d requests with auth %q", unsafeCalls, unsafeAuth)
	}
}

func TestGenerateRejectsEmptyHTTPOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"  "}}]}`))
	}))
	defer srv.Close()
	c, err := New(srv.URL, "", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	var b outbound.Builder
	b.Static("user")
	p, err := b.Build("system")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Generate(context.Background(), p); !errors.Is(err, ErrEmptyOutput) {
		t.Fatalf("empty provider output error = %v", err)
	}
}

type contextBlockingTransport struct{}

func (contextBlockingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestHTTPProviderPropagatesCancellationAndTimeout(t *testing.T) {
	provider, err := New("http://127.0.0.1:1", "", "test-model")
	if err != nil {
		t.Fatal(err)
	}
	provider.HTTP.Transport = contextBlockingTransport{}
	var builder outbound.Builder
	builder.Static("user")
	payload, err := builder.Build("system")
	if err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Generate(canceled, payload); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}

	provider.HTTP.Timeout = 10 * time.Millisecond
	timed, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if _, err := provider.Generate(timed, payload); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
}
