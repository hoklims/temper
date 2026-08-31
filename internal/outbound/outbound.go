// Package outbound is the single gate between local data and any LLM provider. Every dynamic
// value — transcript turns, repo context files, managed AGENTS.md blocks, project metadata —
// enters a request through Builder.Data, which redacts credential shapes, neutralizes control
// markers and bounds size. The assembled payload is redacted once more at Build time, so a caller
// that forgets the discipline still cannot hand a credential-shaped string to a provider.
//
// llm.Provider.Generate accepts only a Payload, whose fields are unexported: there is no way to reach
// the wire from outside this package.
package outbound

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/hoklims/temper/internal/redact"
)

// MaxUserBytes caps the assembled user message. A provider payload is both a cost and an
// exposure; both are bounded here, not left to callers.
const MaxUserBytes = 120000

const truncationMark = "\n…[truncated]\n"

// ErrEmpty is returned when a payload would carry no instruction at all.
var ErrEmpty = errors.New("outbound: refusing to send an empty payload")

var ErrInvalidOutputSchema = errors.New("outbound: invalid output schema")

var ErrInvalidExcludedRoot = errors.New("outbound: invalid excluded root")

// neutralizer defangs the control markers a transcript could use to close Temper's own
// delimiters or to smuggle managed-block syntax into a suggestion body. The text stays readable
// (evidence must survive) but stops being syntax. Every replacement is idempotent: a second pass
// finds nothing left to replace.
var neutralizer = strings.NewReplacer(
	"<transcript>", "[transcript]",
	"</transcript>", "[/transcript]",
	"<!--", "< !--",
	"-->", "-- >",
	"temper:", "temper[:]", "autoskills:", "autoskills[:]",
)

// Sanitize is the transformation every untrusted string undergoes before it can be shown to a
// provider: secrets out, control markers inert. It is idempotent, so applying it at several
// layers is safe — and callers that need the exact text the model saw (evidence verification)
// can reproduce it.
func Sanitize(s string) string {
	return neutralizer.Replace(redact.Text(s))
}

// Payload is the only value llm.Provider.Generate accepts. It can be produced solely by Builder.Build.
type Payload struct {
	system string
	user   string
	schema string
	roots  []string
}

func (p Payload) System() string          { return p.system }
func (p Payload) User() string            { return p.user }
func (p Payload) OutputSchema() string    { return p.schema }
func (p Payload) ExcludedRoots() []string { return append([]string(nil), p.roots...) }

// Builder assembles a prompt out of two kinds of text, and only two:
//
//   - Static: code-owned literals (instructions, labels, delimiters). Never a runtime value.
//   - Data:   untrusted dynamic content. Redacted, neutralized and capped on the way in.
//
// Build may be called repeatedly; it does not consume the builder, so a corrective retry can
// append a static reminder and rebuild.
type Builder struct {
	sb       strings.Builder
	overflow bool
}

// Static appends a code-owned literal. It takes no format arguments on purpose: interpolating a
// runtime value here would bypass the redaction boundary.
func (b *Builder) Static(s string) *Builder {
	b.write(s)
	return b
}

// Data appends untrusted dynamic content, sanitized and capped at limit bytes (limit <= 0 means
// only the global payload cap applies).
func (b *Builder) Data(s string, limit int) *Builder {
	s = Sanitize(s)
	if limit > 0 && len(s) > limit {
		s = cutBytes(s, limit) + truncationMark
	}
	b.write(s)
	return b
}

func (b *Builder) write(s string) {
	if b.overflow {
		return
	}
	if b.sb.Len()+len(s) > MaxUserBytes {
		if room := MaxUserBytes - b.sb.Len(); room > 0 {
			b.sb.WriteString(cutBytes(s, room))
		}
		b.sb.WriteString(truncationMark)
		b.overflow = true
		return
	}
	b.sb.WriteString(s)
}

// Build closes the payload. system must be a code-owned constant; the user message carries every
// dynamic byte and is redacted one final time — the defense that makes the boundary an invariant
// rather than a convention.
func (b *Builder) Build(system string) (Payload, error) {
	return b.build(system, "")
}

func (b *Builder) BuildWithOutputSchema(system, schema string, excludedRoots ...string) (Payload, error) {
	schema = Sanitize(schema)
	if !json.Valid([]byte(schema)) {
		return Payload{}, ErrInvalidOutputSchema
	}
	roots := make([]string, 0, len(excludedRoots))
	for _, root := range excludedRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		absolute, err := filepath.Abs(root)
		if err != nil {
			return Payload{}, fmt.Errorf("%w: %v", ErrInvalidExcludedRoot, err)
		}
		roots = append(roots, filepath.Clean(absolute))
	}
	payload, err := b.build(system, schema)
	if err != nil {
		return Payload{}, err
	}
	payload.roots = roots
	return payload, nil
}

func (b *Builder) build(system, schema string) (Payload, error) {
	system = Sanitize(system)
	user := redact.Text(b.sb.String())
	if strings.TrimSpace(system) == "" || strings.TrimSpace(user) == "" {
		return Payload{}, ErrEmpty
	}
	return Payload{system: system, user: user, schema: schema}, nil
}

// cutBytes truncates to at most n bytes without splitting a UTF-8 rune.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
