package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoklims/temper/internal/store"
)

// bootstrapCapability exercises the real credential path a legitimate UI uses. Reaching into the
// struct field instead would prove the guard against a token the browser could never obtain.
func bootstrapCapability(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + "/api/capability")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capability bootstrap status = %d", resp.StatusCode)
	}
	var payload struct {
		Capability string `json:"capability"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Capability == "" {
		t.Fatal("bootstrap returned an empty capability")
	}
	return payload.Capability
}

// hostileRequest builds a decision request whose authority pieces are set individually, so each
// test removes exactly one and keeps every other one valid.
func hostileRequest(t *testing.T, ts *httptest.Server, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/suggestions/sg_int01/decision", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// assertNothingHappened is the oracle every hostile case shares: the refusal must be visible in the
// database and on disk, not only in the status code.
func assertNothingHappened(t *testing.T, st *store.Store, repo string) {
	t.Helper()
	g, err := st.GetSuggestion("sg_int01")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "pending" {
		t.Fatalf("refused request decided the suggestion: status = %q", g.Status)
	}
	if g.WrittenPath != "" {
		t.Fatalf("refused request recorded a written path: %q", g.WrittenPath)
	}
	if _, err := os.Stat(filepath.Join(repo, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("refused request wrote an artifact: %v", err)
	}
}

func TestDecisionRefusesHostileOriginsAndBodies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		want    int
		mutate  func(*http.Request, string)
		payload string
	}{
		{
			name: "cross_origin_page",
			want: http.StatusForbidden,
			mutate: func(r *http.Request, capability string) {
				r.Header.Set(CapabilityHeader, capability)
				r.Header.Set("Origin", "http://evil.example")
			},
		},
		{
			name: "cross_site_fetch_metadata",
			want: http.StatusForbidden,
			mutate: func(r *http.Request, capability string) {
				r.Header.Set(CapabilityHeader, capability)
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			},
		},
		{
			name: "dns_rebinding_host",
			want: http.StatusForbidden,
			mutate: func(r *http.Request, capability string) {
				// the attacker's own name resolved to this socket, so Origin and Host agree —
				// only the fact that this process never bound that name refuses it
				r.Header.Set(CapabilityHeader, capability)
				r.Host = "rebound.evil.example"
				r.Header.Set("Origin", "http://rebound.evil.example")
			},
		},
		{
			name:   "missing_capability",
			want:   http.StatusForbidden,
			mutate: func(r *http.Request, string string) {},
		},
		{
			name: "wrong_capability",
			want: http.StatusForbidden,
			mutate: func(r *http.Request, capability string) {
				forged := []byte(capability)
				forged[0] ^= 0xff
				r.Header.Set(CapabilityHeader, string(forged))
			},
		},
		{
			name: "non_json_content_type",
			want: http.StatusUnsupportedMediaType,
			mutate: func(r *http.Request, capability string) {
				r.Header.Set(CapabilityHeader, capability)
				r.Header.Set("Content-Type", "text/plain")
			},
		},
		{
			name: "form_content_type",
			want: http.StatusUnsupportedMediaType,
			mutate: func(r *http.Request, capability string) {
				r.Header.Set(CapabilityHeader, capability)
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			},
		},
		{
			name:    "oversized_body",
			want:    http.StatusRequestEntityTooLarge,
			payload: `{"action":"accept","body":"` + strings.Repeat("x", maxDecisionBodyBytes+1024) + `"}`,
			mutate: func(r *http.Request, capability string) {
				r.Header.Set(CapabilityHeader, capability)
			},
		},
		{
			name:    "unknown_field",
			want:    http.StatusBadRequest,
			payload: `{"action":"accept","autoAccept":true}`,
			mutate: func(r *http.Request, capability string) {
				r.Header.Set(CapabilityHeader, capability)
			},
		},
		{
			name:    "trailing_json",
			want:    http.StatusBadRequest,
			payload: `{"action":"reject"}{"action":"accept"}`,
			mutate: func(r *http.Request, capability string) {
				r.Header.Set(CapabilityHeader, capability)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, st, repo := newTestServer(t)
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			capability := bootstrapCapability(t, ts)

			payload := tc.payload
			if payload == "" {
				payload = `{"action":"accept"}`
			}
			req := hostileRequest(t, ts, payload)
			tc.mutate(req, capability)
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			assertNothingHappened(t, st, repo)
		})
	}
}

// The capability is the mutation authority, so handing it out is itself an authority boundary: a
// page that is not same-origin must not be able to read it and then replay it.
func TestCapabilityBootstrapRefusesHostileCallers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "cross_origin", mutate: func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }},
		{name: "opaque_origin", mutate: func(r *http.Request) { r.Header.Set("Origin", "null") }},
		{name: "cross_site", mutate: func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }},
		{name: "rebinding_host", mutate: func(r *http.Request) { r.Host = "rebound.evil.example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			legitimate := bootstrapCapability(t, ts)

			req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/capability", nil)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(req)
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
			raw := make([]byte, 4096)
			n, _ := resp.Body.Read(raw)
			if strings.Contains(string(raw[:n]), legitimate) {
				t.Fatal("refused bootstrap still disclosed the capability")
			}
		})
	}
}

// A rebinding page must not be able to read the inbox either: the suggestions are private data,
// and the same-origin boundary is applied to reads for that reason.
func TestReadsRefuseRebindingHost(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, path := range []string{"/api/suggestions?status=pending", "/api/stats", "/api/projects"} {
		req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "rebound.evil.example"
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 4096)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403", path, resp.StatusCode)
		}
		if strings.Contains(string(body[:n]), "sg_int01") {
			t.Fatalf("%s disclosed suggestion data to a rebinding host", path)
		}
	}
}

// Widening the listener is an operator decision about where the socket lives. It must not become a
// decision about who may accept a suggestion.
func TestWidenedListenerKeepsDecisionAuthorityLocal(t *testing.T) {
	srv, st, repo := newTestServer(t)
	srv.Addr = "0.0.0.0:4517"
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	capability := bootstrapCapability(t, ts)

	hostile := hostileRequest(t, ts, `{"action":"accept"}`)
	hostile.Header.Set(CapabilityHeader, capability)
	hostile.Host = "rebound.evil.example"
	resp, err := ts.Client().Do(hostile)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("widened listener accepted a rebinding host: status = %d", resp.StatusCode)
	}
	assertNothingHappened(t, st, repo)

	// and the legitimate local UI still works through the same widened listener
	legit := hostileRequest(t, ts, `{"action":"accept"}`)
	legit.Header.Set(CapabilityHeader, capability)
	legit.Host = "localhost:4517"
	legit.Header.Set("Origin", "http://localhost:4517")
	legit.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err = ts.Client().Do(legit)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legitimate same-origin decision status = %d", resp.StatusCode)
	}
	g, err := st.GetSuggestion("sg_int01")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "accepted" {
		t.Fatalf("legitimate decision did not land: status = %q", g.Status)
	}
	if _, err := os.Stat(filepath.Join(repo, "AGENTS.md")); err != nil {
		t.Fatalf("legitimate decision wrote nothing: %v", err)
	}
}

// An interface the operator named explicitly is a host this process recognizes — the guard is a
// boundary, not a blanket loopback rule.
func TestExplicitlyBoundHostIsAccepted(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.Addr = "192.168.1.5:4517"
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	capability := bootstrapCapability(t, ts)

	req := hostileRequest(t, ts, `{"action":"reject"}`)
	req.Header.Set(CapabilityHeader, capability)
	req.Host = "192.168.1.5:4517"
	req.Header.Set("Origin", "http://192.168.1.5:4517")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("explicitly bound host status = %d", resp.StatusCode)
	}
	g, err := st.GetSuggestion("sg_int01")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "rejected" {
		t.Fatalf("status = %q", g.Status)
	}
}

func TestCapabilityIsUnpredictableAndProcessScoped(t *testing.T) {
	first, _, _ := newTestServer(t)
	firstServer := httptest.NewServer(first.Handler())
	defer firstServer.Close()
	second, _, _ := newTestServer(t)
	secondServer := httptest.NewServer(second.Handler())
	defer secondServer.Close()

	a := bootstrapCapability(t, firstServer)
	b := bootstrapCapability(t, secondServer)
	if a == b {
		t.Fatal("two servers issued the same capability")
	}
	if len(a) < 32 {
		t.Fatalf("capability is too short to be unpredictable: %d chars", len(a))
	}
	if bootstrapCapability(t, firstServer) != a {
		t.Fatal("capability changed within one process")
	}
}

func TestLegacyCapabilityHeaderRemainsAcceptedDuringUpgrade(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req := hostileRequest(t, ts, `{"action":"reject"}`)
	req.Header.Set(LegacyCapabilityHeader, bootstrapCapability(t, ts))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy capability status = %d", resp.StatusCode)
	}
	g, err := st.GetSuggestion("sg_int01")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "rejected" {
		t.Fatalf("status = %q", g.Status)
	}
}
