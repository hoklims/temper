// Package cursor reads Cursor agent transcripts:
// ~/.cursor/projects/<mangled-path>/agent-transcripts/<session-uuid>/<session-uuid>.jsonl
// Subagent transcripts (subagents/*.jsonl) are intentionally skipped — they are machine-to-machine
// noise with near-zero user-correction signal.
package cursor

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
	Root string // ~/.cursor/projects
}

func New(root string) *Adapter { return &Adapter{Root: root} }

func (a *Adapter) Tool() string { return "cursor" }

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
		tdir := filepath.Join(a.Root, p.Name(), "agent-transcripts")
		sessions, err := os.ReadDir(tdir)
		if err != nil {
			continue
		}
		for _, s := range sessions {
			if !s.IsDir() {
				continue
			}
			// the main transcript is <session-uuid>/<session-uuid>.jsonl
			f := filepath.Join(tdir, s.Name(), s.Name()+".jsonl")
			if st, err := os.Stat(f); err == nil && st.Size() > 0 {
				files = append(files, f)
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return mtime(files[i]).After(mtime(files[j])) })
	return files, nil
}

// transcript line shape: {"role":"user"|"assistant","message":{"content":[{"type":"text","text":"..."}]}}
type line struct {
	Role    string `json:"role"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

func (a *Adapter) Parse(path string) (*canon.Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	// path = <root>/<mangled>/agent-transcripts/<uuid>/<uuid>.jsonl
	mangled := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	repoRoot := ResolveMangledPath(mangled)
	project := mangled
	if repoRoot != "" {
		project = filepath.Base(repoRoot)
	}

	st, _ := f.Stat()
	sess := &canon.Session{
		ID:       sessionID,
		Tool:     "cursor",
		Project:  project,
		RepoRoot: repoRoot,
		Path:     path,
	}
	if st != nil {
		sess.StartedAt = st.ModTime()
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue // tolerate malformed/partial lines
		}
		var b strings.Builder
		for _, c := range l.Message.Content {
			if c.Type == "text" && c.Text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(c.Text)
			}
		}
		text := strings.TrimSpace(b.String())
		if text == "" {
			continue
		}
		role := canon.RoleAssistant
		if l.Role == "user" {
			role = canon.RoleUser
		}
		sess.Turns = append(sess.Turns, canon.Turn{Role: role, Text: text})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return sess, nil
}

// ResolveMangledPath converts Cursor's flattened project dir name ("Users-elcruzo-Desktop-Trace")
// back into an absolute path by DFS over the filesystem: at each level, consecutive dash-joined
// segments may either descend into a directory or be part of a directory name containing dashes
// (e.g. "YC-Hackathon"). Returns "" when no existing path matches.
func ResolveMangledPath(mangled string) string {
	if strings.TrimSpace(mangled) == "" {
		return ""
	}
	return resolveFrom("/", strings.Split(mangled, "-"))
}

func resolveFrom(base string, parts []string) string {
	if len(parts) == 0 {
		return base
	}
	// Greedily try the shortest dir name first (most common), then longer dash-joined names.
	name := ""
	for i := 0; i < len(parts); i++ {
		if i == 0 {
			name = parts[0]
		} else {
			name += "-" + parts[i]
		}
		candidate := filepath.Join(base, name)
		if isDir(candidate) {
			if r := resolveFrom(candidate, parts[i+1:]); r != "" {
				return r
			}
		}
	}
	return ""
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func mtime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}
