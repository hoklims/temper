package redact

import (
	"strings"
	"testing"
)

func TestRedactsKnownTokenShapes(t *testing.T) {
	cases := []string{
		"my key is sk-proj-abc123def456ghi789jkl012mno345",
		"export ANTHROPIC_API_KEY=sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaa",
		"token ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA done",
		"aws AKIAIOSFODNN7EXAMPLE here",
		`{"api_key": "supersecretvalue123"}`,
		"Authorization: Bearer abcdefgh12345678",
	}
	for _, c := range cases {
		got := Text(c)
		if got == c {
			t.Errorf("not redacted: %q", c)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("missing placeholder in %q -> %q", c, got)
		}
	}
}

// The hostile corpus HOK-539 requires: nothing below may survive a trip to a provider.
func TestRedactsHostileCorpus(t *testing.T) {
	stripeFixture := "sk_" + "live_" + "51H8xQrLkdIwHu7ix0987654321"
	slackFixture := "https://hooks.slack.com/" + "services/" + "T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
	cases := map[string]string{
		"aws temporary key":     "creds ASIAY34FZKBOKMUTVV7A rotated",
		"google oauth":          "Authorization was ya29.a0AfH6SMBx1234567890abcdefghijklmno",
		"jwt":                   "cookie=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
		"slack webhook":         "post to " + slackFixture,
		"postgres dsn":          "DATABASE_URL=postgres://appuser:s3cr3tp4ss@db.example.com:5432/app",
		"mongo dsn":             "mongodb+srv://admin:LetMeIn123@cluster0.mongodb.net/test",
		"azure connstring":      "DefaultEndpointsProtocol=https;AccountName=x;AccountKey=Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MA==;",
		"env file line":         "STRIPE_SECRET_KEY=" + stripeFixture,
		"gcp service account":   `"private_key": "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\n-----END PRIVATE KEY-----"`,
		"bare bearer":           "curl -H 'Bearer eyJhbGciOiJIUzI1NiJ9abcdefgh' https://api.example.com",
		"internal http url":     "the staging box is at http://10.12.4.7:8080/admin",
		"internal tld url":      "docs live on https://wiki.corp.internal/runbooks/deploy",
		"generic credential":    "credential = supersecretvalue123",
		"password in ini":       "password: hunter2hunter2",
		"private key assigned":  "private_key=MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQ",
		"access key assignment": "access_key = AKIAIOSFODNN7EXAMPLE",
		"email address":         "contact guillaume@example.com before release",
		"internal label host":   "deploy through https://api.corp.example.com/runbook",
	}
	// the concrete substrings that must not survive
	forbidden := map[string]string{
		"aws temporary key":     "ASIAY34FZKBOKMUTVV7A",
		"google oauth":          "ya29.a0AfH6SMBx1234567890abcdefghijklmno",
		"jwt":                   "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0",
		"slack webhook":         "T00000000/B00000000",
		"postgres dsn":          "s3cr3tp4ss",
		"mongo dsn":             "LetMeIn123",
		"azure connstring":      "Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MA==",
		"env file line":         stripeFixture,
		"gcp service account":   "MIIEvQIBADANBg",
		"bare bearer":           "eyJhbGciOiJIUzI1NiJ9abcdefgh",
		"internal http url":     "10.12.4.7:8080/admin",
		"internal tld url":      "wiki.corp.internal/runbooks/deploy",
		"generic credential":    "supersecretvalue123",
		"password in ini":       "hunter2hunter2",
		"private key assigned":  "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQ",
		"access key assignment": "AKIAIOSFODNN7EXAMPLE",
		"email address":         "guillaume@example.com",
		"internal label host":   "api.corp.example.com/runbook",
	}
	for name, in := range cases {
		got := Text(in)
		if leak := forbidden[name]; strings.Contains(got, leak) {
			t.Errorf("%s: %q survived redaction: %q", name, leak, got)
		}
		if !strings.Contains(got, placeholder) {
			t.Errorf("%s: nothing was redacted in %q -> %q", name, in, got)
		}
	}
}

func TestKeepsLoopbackAndNormalUrls(t *testing.T) {
	// loopback is dev noise, not an asset — redacting it would gut the evidence corpus
	cases := []string{
		"open http://localhost:3000 to check the fix",
		"the dashboard runs on http://127.0.0.1:4517",
		"see https://github.com/hoklims/temper/issues/12",
	}
	for _, c := range cases {
		if got := Text(c); got != c {
			t.Errorf("over-redacted: %q -> %q", c, got)
		}
	}
}

func TestKeepsNormalText(t *testing.T) {
	cases := []string{
		"run pnpm install then pnpm build",
		"the file src/components/Header.tsx has the bug",
		"git commit -m 'fix: resolve linkage'",
		"a normal sentence with no secrets at all",
	}
	for _, c := range cases {
		if got := Text(c); got != c {
			t.Errorf("over-redacted: %q -> %q", c, got)
		}
	}
}

func TestRedactsPrivateKeyBlock(t *testing.T) {
	in := "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIEow...\n-----END RSA PRIVATE KEY-----\nafter"
	got := Text(in)
	if strings.Contains(got, "MIIEow") {
		t.Errorf("private key not redacted: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("surrounding text lost: %q", got)
	}
}
