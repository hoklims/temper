// Package llm provides inference through HTTP chat completions or authenticated official CLIs.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hoklims/temper/internal/outbound"
)

type Client struct {
	Endpoint string // base URL, e.g. https://api.anthropic.com/v1
	APIKey   string
	Model    string
	HTTP     *http.Client
}

type Provider interface {
	Generate(context.Context, outbound.Payload) (string, error)
}

// New validates the endpoint before returning a client: no request carrying the provider key is
// ever built against an unvetted destination.
func New(endpoint, apiKey, model string) (*Client, error) {
	ep := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if err := validateHTTPConfiguration(ep, apiKey); err != nil {
		return nil, err
	}
	return &Client{
		Endpoint: ep,
		APIKey:   apiKey,
		Model:    model,
		HTTP:     &http.Client{Timeout: 180 * time.Second},
	}, nil
}

func validateHTTPConfiguration(endpoint, apiKey string) error {
	if err := ValidateEndpoint(endpoint); err != nil {
		return err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("llm: endpoint is not a valid URL")
	}
	if strings.TrimSpace(apiKey) == "" && !isLoopback(parsed.Hostname()) {
		return fmt.Errorf("llm: no API key configured for remote HTTP provider")
	}
	return nil
}

// ValidateEndpoint bounds where the provider key may travel: TLS for anything remote, plaintext
// HTTP only to loopback (local Ollama and friends), no credentials smuggled in the URL, no
// ambiguous scheme or destination. Errors never echo the raw URL — it may carry a password.
func ValidateEndpoint(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("llm: no endpoint configured")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("llm: endpoint is not a valid URL")
	}
	if u.Opaque != "" {
		return fmt.Errorf("llm: endpoint must be an absolute http(s) URL")
	}
	if u.User != nil {
		return fmt.Errorf("llm: endpoint must not carry credentials in the URL (userinfo before %q)", u.Hostname())
	}
	if u.Fragment != "" {
		return fmt.Errorf("llm: endpoint must not carry a fragment")
	}
	// A base endpoint is a prefix that "/chat/completions" is appended to. With a query already
	// present that concatenation produces "…?key=secret/chat/completions": the path lands inside a
	// query parameter, the request goes somewhere nobody configured, and a credential smuggled in
	// the query travels with it. There is no correct way to append a path after a query, so the
	// ambiguity is refused instead of resolved. An explicit api_key remains the only credential
	// path for a custom gateway.
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("llm: endpoint must not carry a query string — configure credentials with api_key, not in the URL")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("llm: endpoint has no host")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if isLoopback(host) {
			return nil
		}
		return fmt.Errorf("llm: refusing plaintext http to non-loopback host %q — use https for a remote endpoint", host)
	default:
		return fmt.Errorf("llm: unsupported endpoint scheme %q — only https, or http on loopback", u.Scheme)
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate sends a prepared payload and returns the assistant text. Retries transient failures.
// The parameter type is the boundary: only internal/outbound can produce a Payload, so nothing
// unredacted can reach the request body from here.
func (c *Client) Generate(ctx context.Context, p outbound.Payload) (string, error) {
	if err := validateHTTPConfiguration(c.Endpoint, c.APIKey); err != nil {
		return "", err
	}
	req := chatRequest{
		Model:       c.Model,
		Temperature: 0.2,
		MaxTokens:   8192,
		Messages: []Message{
			{Role: "system", Content: p.System()},
			{Role: "user", Content: p.User()},
		},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
		}
		out, retryable, err := c.once(ctx, body)
		if err == nil && strings.TrimSpace(out) == "" {
			return "", ErrEmptyOutput
		}
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retryable {
			break
		}
	}
	return "", lastErr
}

func (c *Client) once(ctx context.Context, body []byte) (out string, retryable bool, err error) {
	// Re-checked here, not only in New: a Client built as a struct literal (tests, future callers)
	// must not be able to put the provider key on the wire toward an unvetted destination.
	if err := ValidateEndpoint(c.Endpoint); err != nil {
		return "", false, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		// Anthropic's OpenAI-compat layer accepts Authorization; the native header is harmless elsewhere.
		httpReq.Header.Set("x-api-key", c.APIKey)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 180 * time.Second}
	}
	// Redirects are not part of the configured provider authority. In particular, a 307/308
	// would replay the POST body and provider headers to a destination that never passed the
	// endpoint policy. Clone the client so even an injected/custom client cannot weaken this gate.
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := safeClient.Do(httpReq)
	if err != nil {
		return "", true, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", true, err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return "", true, fmt.Errorf("llm: %s: %s", resp.Status, truncate(string(raw), 300))
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("llm: %s: %s", resp.Status, truncate(string(raw), 300))
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", false, fmt.Errorf("llm: decode response: %w", err)
	}
	if cr.Error != nil {
		return "", false, fmt.Errorf("llm: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", false, fmt.Errorf("llm: empty choices")
	}
	return cr.Choices[0].Message.Content, false, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
