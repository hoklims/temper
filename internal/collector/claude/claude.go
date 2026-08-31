// Package claude reads Claude Code transcripts: ~/.claude/projects/<mangled>/<session-uuid>.jsonl.
// Each meaningful line carries type user/assistant plus a cwd field, which gives us the real repo
// root for free (no path de-mangling needed). Sidechain (subagent) lines are skipped.
package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hoklims/temper/internal/canon"
)

type Adapter struct {
	Root string // ~/.claude/projects
}

func New(root string) *Adapter { return &Adapter{Root: root} }

func (a *Adapter) Tool() string { return "claude" }

func (a *Adapter) SessionFiles() ([]string, error) {
	projects, err := os.ReadDir(a.Root)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(a.Root, p.Name()))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			f := filepath.Join(a.Root, p.Name(), e.Name())
			if st, err := os.Stat(f); err == nil && st.Size() > 0 {
				files = append(files, f)
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return mtime(files[i]).After(mtime(files[j])) })
	return files, nil
}

// line covers the union of shapes we care about. message.content is either a plain string
// (user prompts) or an array of typed blocks (assistant output, tool results).
type line struct {
	Type        string          `json:"type"`
	IsSidechain bool            `json:"isSidechain"`
	CWD         string          `json:"cwd"`
	Timestamp   string          `json:"timestamp"`
	Message     json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (a *Adapter) Parse(path string) (*canon.Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sess := &canon.Session{
		ID:   strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		Tool: "claude",
		Path: path,
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		if l.IsSidechain || (l.Type != "user" && l.Type != "assistant") {
			continue
		}
		if sess.RepoRoot == "" && l.CWD != "" {
			sess.RepoRoot = l.CWD
			sess.Project = filepath.Base(l.CWD)
		}
		if sess.StartedAt.IsZero() && l.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339, l.Timestamp); err == nil {
				sess.StartedAt = t
			}
		}
		var m message
		if err := json.Unmarshal(l.Message, &m); err != nil {
			continue
		}
		text := extractText(m.Content)
		if text == "" {
			continue
		}
		role := canon.RoleAssistant
		if m.Role == "user" {
			role = canon.RoleUser
		}
		// tool_result payloads arrive as type=user lines; genuine prompts are plain strings.
		// Keep both but classify structured user content as tool output.
		if m.Role == "user" && !isPlainString(m.Content) {
			role = canon.RoleTool
		}
		sess.Turns = append(sess.Turns, canon.Turn{Role: role, Text: text})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if sess.StartedAt.IsZero() {
		sess.StartedAt = mtime(path)
	}
	if sess.Project == "" {
		sess.Project = filepath.Base(filepath.Dir(path))
	}
	return sess, nil
}

func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" && blk.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(blk.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func isPlainString(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil
}

func mtime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
