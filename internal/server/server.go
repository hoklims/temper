// Package server exposes the local review API and serves the embedded dashboard. It defaults to
// 127.0.0.1 — this is the "web dashboard with no server" (PRD §10.2) — but the listen address is
// an operator choice, so the authority to decide a suggestion is not derived from it: it comes
// from a process-scoped capability handed only to a same-origin local UI.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/hoklims/temper/internal/store"
	"github.com/hoklims/temper/internal/writer"
	"github.com/hoklims/temper/web"
)

const DefaultAddr = "127.0.0.1:4517"

// CapabilityHeader carries the process-scoped capability that authorizes a mutation. A custom
// header is deliberate: a browser cannot send one cross-origin without a preflight, and no CORS
// response header is ever emitted, so the preflight fails before the request is made.
const CapabilityHeader = "X-Temper-Capability"

// LegacyCapabilityHeader keeps already-open AutoSkills dashboards working during an in-place
// upgrade. New clients receive and send CapabilityHeader.
const LegacyCapabilityHeader = "X-AutoSkills-Capability"

// maxDecisionBodyBytes bounds a decision request before it is decoded. An edited skill body is
// review-sized text, not an upload.
const maxDecisionBodyBytes = 1 << 20

type Server struct {
	Store *store.Store
	// Addr is the address the operator asked the listener to bind. Its hostname, when it names a
	// concrete interface, is the only non-loopback name a browser may present in Host. A wildcard
	// address widens *where the socket listens*, never *who may decide*.
	Addr string

	setupOnce sync.Once
	// capability is generated once per process and never persisted. Empty means the generator
	// failed, and every mutation then fails closed.
	capability string
	// configuredHost is the lowercased hostname taken from Addr, empty for a wildcard.
	configuredHost string
}

func (s *Server) setup() {
	s.setupOnce.Do(func() {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err == nil {
			s.capability = hex.EncodeToString(raw)
		}
		s.configuredHost = configuredHostname(s.Addr)
	})
}

func (s *Server) Handler() http.Handler {
	s.setup()
	api := http.NewServeMux()
	api.HandleFunc("GET /api/capability", s.handleCapability)
	api.HandleFunc("GET /api/stats", s.handleStats)
	api.HandleFunc("GET /api/suggestions", s.handleListSuggestions)
	api.HandleFunc("POST /api/suggestions/{id}/decision", s.handleDecision)
	api.HandleFunc("GET /api/projects", s.handleProjects)

	mux := http.NewServeMux()
	mux.Handle("/api/", s.sameOriginOnly(api))
	mux.Handle("/", uiHandler())
	return mux
}

// sameOriginOnly is the browser boundary, applied to every API route including the reads: a page
// that reached this process under a name it chose — DNS rebinding — must not be able to read the
// inbox either. It is not the mutation authority; that is the capability.
func (s *Server) sameOriginOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.setup()
		w.Header().Set("Vary", "Origin")
		if !s.hostAllowed(r.Host) {
			http.Error(w, "forbidden: unexpected Host", http.StatusForbidden)
			return
		}
		if !sameOriginRequest(r) {
			http.Error(w, "forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// configuredHostname extracts the hostname an operator explicitly bound. A wildcard address names
// no host, so it grants no name.
func configuredHostname(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return ""
	}
	return host
}

// hostAllowed answers the DNS-rebinding question: did the browser reach this process under a name
// this process recognizes? An attacker controls the hostname their victim's browser resolves, so a
// name that is neither loopback nor the one the operator bound is refused — the fact that it
// resolved to this socket proves nothing.
func (s *Server) hostAllowed(header string) bool {
	if header == "" {
		return false
	}
	host := header
	if h, _, err := net.SplitHostPort(header); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return s.configuredHost != "" && host == s.configuredHost
}

// sameOriginRequest refuses anything a browser labels as coming from elsewhere. A request with no
// Origin and no fetch metadata is a non-browser client (curl, the CLI): it is allowed through this
// gate and still has to present the capability to mutate anything.
func sameOriginRequest(r *http.Request) bool {
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "", "same-origin", "none":
	default:
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return false // "null" and opaque origins included
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

// authorized is the mutation authority. It is a shared secret held in this process's memory and
// handed only to a same-origin bootstrap, so possessing it is evidence of being the local UI.
func (s *Server) authorized(r *http.Request) bool {
	s.setup()
	if s.capability == "" {
		return false
	}
	presented := r.Header.Get(CapabilityHeader)
	if presented == "" {
		presented = r.Header.Get(LegacyCapabilityHeader)
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.capability)) == 1
}

// handleCapability hands the running process's capability to a page that already proved it is
// same-origin. It is never persisted and never cached.
func (s *Server) handleCapability(w http.ResponseWriter, r *http.Request) {
	s.setup()
	if s.capability == "" {
		http.Error(w, "capability unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"capability": s.capability})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, st)
}

func (s *Server) handleListSuggestions(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}
	list, err := s.Store.ListSuggestions(status)
	if err != nil {
		writeErr(w, err)
		return
	}
	if list == nil {
		list = []store.Suggestion{}
	}
	writeJSON(w, map[string]any{"suggestions": list})
}

type decisionRequest struct {
	Action string `json:"action"` // accept | reject
	Body   string `json:"body"`   // optional edited markdown (accept only)
}

// hasJSONContentType requires the declared type, not a guess at the bytes. A body a browser can
// send cross-origin without a preflight (form, text, multipart) is refused by name.
func hasJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

// decodeDecisionRequest reads a bounded, closed, single JSON object. Unknown fields and trailing
// JSON are refused rather than ignored: a request this handler does not fully understand is not a
// request it should act on.
func decodeDecisionRequest(w http.ResponseWriter, r *http.Request) (decisionRequest, error) {
	var req decisionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxDecisionBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return decisionRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return decisionRequest{}, errors.New("server: trailing content after decision request")
		}
		return decisionRequest{}, err
	}
	return req, nil
}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	// Authority, shape and size are all settled before the store is read: a refused request must
	// not be able to observe or move anything, not even by racing a lookup.
	if !s.authorized(r) {
		http.Error(w, "forbidden: missing or invalid capability", http.StatusForbidden)
		return
	}
	if !hasJSONContentType(r) {
		http.Error(w, "expected Content-Type: application/json", http.StatusUnsupportedMediaType)
		return
	}
	id := r.PathValue("id")
	req, err := decodeDecisionRequest(w, r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	g, err := s.Store.GetSuggestion(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if g.Status != "pending" {
		http.Error(w, "suggestion is no longer pending", http.StatusConflict)
		return
	}

	switch req.Action {
	case "reject":
		// the status check above is a courtesy: Reject is a compare-and-set from pending inside a
		// transaction, so a rejection racing an acceptance loses instead of contradicting it
		if err := s.Store.Reject(id); err != nil {
			writeDecisionErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "accept":
		if req.Body != "" {
			g.Body = req.Body
		}
		// The plan is validated on the body that will actually be written, edits included. A
		// suggestion whose artifact plan does not resolve is a client error: nothing is written
		// and the decision is not recorded.
		if _, err := writer.BuildPlan(g); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Accept journals the mutation, writes the files, then records the decision in the same
		// transaction that closes the journal entry — the status above is a courtesy 409, the
		// binding pending-only check happens inside that transaction.
		written, err := writer.Accept(s.Store, g)
		if err != nil {
			writeDecisionErr(w, fmt.Errorf("write skill: %w", err))
			return
		}
		writeJSON(w, map[string]any{"ok": true, "writtenPath": written})
	default:
		http.Error(w, "action must be accept or reject", http.StatusBadRequest)
	}
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.Projects()
	if err != nil {
		writeErr(w, err)
		return
	}
	if projects == nil {
		projects = []store.ProjectCount{}
	}
	writeJSON(w, map[string]any{"projects": projects})
}

// uiHandler serves the embedded dashboard build with an index.html SPA fallback.
func uiHandler() http.Handler {
	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return placeholderHandler()
	}
	if _, err := fs.Stat(dist, "index.html"); err != nil {
		return placeholderHandler()
	}
	fileServer := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(p, "api/") {
			http.NotFound(w, r) // unknown/wrong-method API routes must not get 200 HTML
			return
		}
		if p != "" {
			if _, err := fs.Stat(dist, p); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		// SPA fallback
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

func placeholderHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "temper: dashboard assets not built. run `bun run build` in web/ and rebuild the binary.")
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// writeDecisionErr separates "you lost a race" from "something broke". Both clients of a contested
// suggestion must be able to tell those apart: one reloads and decides again, the other has a bug
// or a broken database to report. Losing a compare-and-set, hitting an unfinished operation, and
// finding a destination changed underneath a manifest are all the first kind.
func writeDecisionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotPending),
		errors.Is(err, store.ErrOperationInFlight),
		errors.Is(err, store.ErrResourceBusy),
		errors.Is(err, writer.ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		writeErr(w, err)
	}
}
