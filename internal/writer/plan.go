package writer

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/hoklims/temper/internal/config"
	"github.com/hoklims/temper/internal/store"
)

// Kind enumerates the artifacts the writer knows how to produce. There is no other destination.
type Kind string

const (
	KindAgentsBlock  Kind = "agents_block"
	KindCursorRule   Kind = "cursor_rule"
	KindRepoSkill    Kind = "repo_skill"
	KindMachineSkill Kind = "machine_skill"
)

// Writer-side bounds. Looser than the distiller's schema on body size because a human may expand
// a body in the review editor — but still bounded, and still the same closed set of destinations.
const (
	maxPlanTitleBytes = 200
	maxPlanBodyBytes  = 20000
	maxPlanGlobs      = 10
	maxPlanGlobBytes  = 200
)

// managedMarkers must never appear in content: a body carrying them could close or forge an
// temper-managed block and take over a region of AGENTS.md it was not granted.
var managedMarkers = []string{
	"temper:begin", "temper:end", "temper:section", "temper:demoted",
	"autoskills:begin", "autoskills:end", "autoskills:section", "autoskills:demoted",
}

// Plan is the deterministic answer to "what exactly would accepting this touch?", computed
// locally from the suggestion — never from anything the model said about where it belongs.
// Write refuses to act without one, so the review UI's preview and the actual mutation are the
// same decision made twice.
type Plan struct {
	Kind Kind
	Path string // absolute destination
	// Root is the only directory this plan is allowed to touch. It travels with the plan into the
	// journal so the confinement check can be repeated at mutation time against the same authority
	// that authorized it, not against a root re-derived from a possibly-tampered row.
	Root    string
	Rel     string // repo-relative (or ~-relative) preview
	BlockID string // managed block this plan targets, for AGENTS.md kinds
	Prune   bool   // true when the plan removes a managed block instead of writing content
}

// BuildPlan validates a suggestion against the closed artifact schema and resolves its
// destination. Every rejection is a refusal to write, never a silent coercion into a default.
func BuildPlan(g store.Suggestion) (Plan, error) {
	scope := strings.ToLower(strings.TrimSpace(g.Scope))
	placement := strings.ToLower(strings.TrimSpace(g.Placement))
	// empty means "unspecified" and keeps the historical default; an unknown VALUE is a hard error
	if scope == "" {
		scope = "repo"
	}
	if placement == "" {
		placement = "always_on"
	}
	if scope != "machine" && scope != "repo" {
		return Plan{}, fmt.Errorf("writer: unsupported scope %q", clip(scope))
	}
	if placement != "always_on" && placement != "path_scoped" && placement != "skill" {
		return Plan{}, fmt.Errorf("writer: unsupported placement %q", clip(placement))
	}
	if math.IsNaN(g.Confidence) || g.Confidence < 0 || g.Confidence > 1 {
		return Plan{}, fmt.Errorf("writer: confidence %v outside [0,1]", g.Confidence)
	}

	title := strings.TrimSpace(g.Title)
	body := strings.TrimSpace(g.Body)
	if len(title) > maxPlanTitleBytes {
		return Plan{}, fmt.Errorf("writer: title of %d bytes exceeds %d", len(title), maxPlanTitleBytes)
	}
	if len(body) > maxPlanBodyBytes {
		return Plan{}, fmt.Errorf("writer: body of %d bytes exceeds %d", len(body), maxPlanBodyBytes)
	}
	if err := checkMarkers("title", title); err != nil {
		return Plan{}, err
	}
	if err := checkMarkers("body", body); err != nil {
		return Plan{}, err
	}
	if placement == "path_scoped" && strings.TrimSpace(g.Globs) == "" {
		return Plan{}, fmt.Errorf("writer: a path_scoped artifact needs at least one glob")
	}
	if placement != "path_scoped" && strings.TrimSpace(g.Globs) != "" {
		return Plan{}, fmt.Errorf("writer: globs are only valid for path_scoped artifacts")
	}
	if err := checkGlobs(g.Globs); err != nil {
		return Plan{}, err
	}

	// machine scope (or an unresolved repo) always lands in the user's own skills dir
	if scope == "machine" || strings.TrimSpace(g.RepoRoot) == "" {
		if title == "" {
			return Plan{}, fmt.Errorf("writer: a machine skill needs a title")
		}
		if body == "" {
			return Plan{}, fmt.Errorf("writer: a machine skill needs a body")
		}
		root := filepath.Join(config.Dir(), "skills")
		path := filepath.Join(root, slug(title)+".md")
		if err := confine(root, path); err != nil {
			return Plan{}, err
		}
		return Plan{Kind: KindMachineSkill, Path: path, Root: root, Rel: "~/.temper/skills/" + slug(title) + ".md"}, nil
	}

	root := g.RepoRoot
	if !filepath.IsAbs(root) {
		return Plan{}, fmt.Errorf("writer: repo root %q is not an absolute path", clip(root))
	}
	root = filepath.Clean(root)

	switch placement {
	case "path_scoped", "skill":
		if title == "" {
			return Plan{}, fmt.Errorf("writer: a %s artifact needs a title", placement)
		}
		if body == "" {
			return Plan{}, fmt.Errorf("writer: a %s artifact needs a body", placement)
		}
		var rel string
		if placement == "path_scoped" {
			rel = filepath.Join(".cursor", "rules", "temper-"+slug(title)+".mdc")
		} else {
			rel = filepath.Join(".cursor", "skills", "temper-"+slug(title), "SKILL.md")
		}
		path := filepath.Join(root, rel)
		if err := confine(root, path); err != nil {
			return Plan{}, err
		}
		kind := KindCursorRule
		if placement == "skill" {
			kind = KindRepoSkill
		}
		return Plan{Kind: kind, Path: path, Root: root, Rel: filepath.ToSlash(rel)}, nil
	}

	// always_on: a managed block inside the repo's AGENTS.md
	path := filepath.Join(root, "AGENTS.md")
	if err := confine(root, path); err != nil {
		return Plan{}, err
	}
	prune := body == ""
	if prune && strings.TrimSpace(g.BlockID) == "" && strings.TrimSpace(g.WrittenPath) == "" {
		// an empty body only means "remove a block" — it must say which one
		return Plan{}, fmt.Errorf("writer: empty body with no block to prune")
	}
	if !prune && title == "" {
		return Plan{}, fmt.Errorf("writer: an AGENTS.md block needs a title")
	}
	return Plan{Kind: KindAgentsBlock, Path: path, Root: root, Rel: "AGENTS.md", BlockID: strings.TrimSpace(g.BlockID), Prune: prune}, nil
}

// TargetPreview returns the destination Write would choose, so pending suggestions can show an
// honest target path in the review UI. Empty when no valid plan exists — which is itself the
// signal that accepting would be refused.
func TargetPreview(g store.Suggestion) string {
	plan, err := BuildPlan(g)
	if err != nil {
		return ""
	}
	return plan.Rel
}

func checkMarkers(field, s string) error {
	lower := strings.ToLower(s)
	for _, m := range managedMarkers {
		if strings.Contains(lower, m) {
			return fmt.Errorf("writer: %s carries the managed marker %q", field, m)
		}
	}
	return nil
}

func checkGlobs(globs string) error {
	globs = strings.TrimSpace(globs)
	if globs == "" {
		return nil
	}
	if strings.ContainsAny(globs, "\n\r\"") {
		return fmt.Errorf("writer: globs contain a newline or quote")
	}
	parts := strings.Split(globs, ",")
	if len(parts) > maxPlanGlobs {
		return fmt.Errorf("writer: %d globs exceed %d", len(parts), maxPlanGlobs)
	}
	for _, p := range parts {
		if len(p) > maxPlanGlobBytes {
			return fmt.Errorf("writer: glob of %d bytes exceeds %d", len(p), maxPlanGlobBytes)
		}
	}
	return nil
}

// confine proves the resolved destination stays under its allowed root — the check that makes a
// traversal attempt a refusal instead of a write outside the managed tree.
func confine(root, path string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("writer: destination is not relative to %q", clip(root))
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("writer: destination escapes %q", clip(root))
	}

	// Lexical containment is not enough: an existing .cursor symlink or Windows junction can
	// redirect a seemingly-contained destination outside the repository. Resolve the deepest
	// existing ancestor of both paths, then re-check containment in canonical space.
	canonicalRoot, err := canonicalPath(root)
	if err != nil {
		return fmt.Errorf("writer: resolve authorized root: %w", err)
	}
	canonicalDestination, err := canonicalPath(path)
	if err != nil {
		return fmt.Errorf("writer: resolve destination: %w", err)
	}
	rel, err = filepath.Rel(canonicalRoot, canonicalDestination)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("writer: destination escapes canonical root %q", clip(canonicalRoot))
	}
	return nil
}

// canonicalPath resolves every existing symlink/junction ancestor and appends only the missing
// suffix. This works before the writer creates .cursor directories and on Windows reparse points.
func canonicalPath(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %q", clip(path))
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// clip bounds a path or token quoted in an error message. It keeps BOTH ends, because the tail of
// a path is what names the file: truncating from the right turns "these files were left as they
// are" into a directory prefix the operator cannot act on, and a rollback that stops has to say
// exactly what it stopped on.
func clip(s string) string {
	const (
		max  = 80
		head = 24
	)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:head]) + "…" + string(r[len(r)-(max-head-1):])
}
