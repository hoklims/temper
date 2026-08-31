// Package writer emits accepted suggestions into the context files agents actually read.
// All writes are idempotent: repo-file content lives inside temper-managed marker blocks
// keyed by suggestion id, so re-accepting updates in place and never clobbers hand-written text.
package writer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hoklims/temper/internal/store"
)

// writeUnjournaled places a suggestion's artifact and returns the path written. It is atomic and
// confined but NOT durable: nothing records that it happened, so a crash mid-mutation leaves a
// state no reconciliation can resolve.
//
// It is deliberately unexported and has no production caller. Accept is the only way in, because
// "write the file, then record the decision" is exactly the two-commit shape this package exists
// to remove — and an exported non-durable door is an invitation to reintroduce it. What remains
// here serves the tests that need a plain filesystem effect without a store.
//
// Sensitivity is deliberately NOT enforced here: the review card surfaces the SENSITIVE badge
// and a human approved the write — the badge is the control (PRD §6.7).
//
// The destination is never read off the suggestion: BuildPlan recomputes it locally and refuses
// anything outside the closed set of artifacts, so an invalid or out-of-tree plan is an error
// rather than a file.
func writeUnjournaled(g store.Suggestion) (string, error) {
	mut, err := BuildMutation(g)
	if err != nil {
		return "", err
	}
	if err := capture(&mut); err != nil {
		return "", err
	}
	if err := applyOps(&mut); err != nil {
		if uErr := unwind(&mut); uErr != nil {
			return "", fmt.Errorf("%w; %v", err, uErr)
		}
		return "", err
	}
	return mut.WrittenPath, nil
}

// BuildMutation computes every file an acceptance would touch and the exact bytes each would
// hold. It reads the repository but writes nothing, so the plan can be journaled — and refused —
// before the first mutation.
//
// Routing:
//   - scope=machine                  -> ~/.temper/skills/<slug>.md
//   - placement=path_scoped (repo)   -> <repo>/.cursor/rules/temper-<slug>.mdc
//   - placement=skill (repo)         -> <repo>/.cursor/skills/temper-<slug>/SKILL.md
//   - placement=always_on (repo)     -> managed block in <repo>/AGENTS.md (+ CLAUDE.md import line if needed)
func BuildMutation(g store.Suggestion) (Mutation, error) {
	plan, err := BuildPlan(g)
	if err != nil {
		return Mutation{}, err
	}
	switch plan.Kind {
	case KindMachineSkill:
		return singleFile(plan, fmt.Sprintf("# %s\n\n%s\n", g.Title, g.Body)), nil
	case KindCursorRule:
		content, err := cursorRuleContent(g)
		if err != nil {
			return Mutation{}, err
		}
		return singleFile(plan, content), nil
	case KindRepoSkill:
		return singleFile(plan, repoSkillContent(g)), nil
	default:
		return planAgentsBlock(g, plan)
	}
}

func singleFile(plan Plan, content string) Mutation {
	return Mutation{
		Ops:         []FileOp{{Root: plan.Root, Path: plan.Path, Content: content}},
		WrittenPath: plan.Path,
	}
}

func cursorRuleContent(g store.Suggestion) (string, error) {
	globs := strings.TrimSpace(g.Globs)
	if globs == "" {
		return "", fmt.Errorf("writer: refusing a path_scoped rule without globs")
	}
	var fm strings.Builder
	fm.WriteString("---\n")
	fm.WriteString("description: " + yamlEscape(g.Title) + "\n")
	// always quoted: bare globs starting with * are YAML aliases and would be invalid
	fm.WriteString("globs: \"" + strings.ReplaceAll(globs, `"`, ``) + "\"\n")
	fm.WriteString("alwaysApply: false\n")
	fm.WriteString("---\n\n")
	return fm.String() + g.Body + "\n", nil
}

// repoSkillContent renders an on-demand skill file. Shell fences in the body stay what they are —
// inert Markdown a human reads and runs deliberately. Temper used to extract them into an
// executable run.sh (0755) next to the skill; that turned model-authored text into a program on
// disk and was removed in HOK-539. No artifact this package writes is executable.
func repoSkillContent(g store.Suggestion) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n", slug(g.Title), yamlEscape(g.Title), g.Body)
}

// AGENTS.md layout: all temper content lives inside ONE managed section, grouped by skill
// kind so related skills read together instead of accumulating chronologically at file end.
const (
	sectionBegin = "<!-- temper:section:begin -->"
	sectionEnd   = "<!-- temper:section:end -->"
)

var (
	blockRes = []*regexp.Regexp{
		regexp.MustCompile(`(?s)<!-- temper:begin id=([^ >]+)(?: group=([a-z]+))?(?: conf=([0-9.]+))? -->\n?(.*?)\n?<!-- temper:end id=[^ >]+ -->\n?`),
		regexp.MustCompile(`(?s)<!-- autoskills:begin id=([^ >]+)(?: group=([a-z]+))?(?: conf=([0-9.]+))? -->\n?(.*?)\n?<!-- autoskills:end id=[^ >]+ -->\n?`),
	}
	sectionRes = []*regexp.Regexp{
		regexp.MustCompile(`(?s)\n?` + regexp.QuoteMeta(sectionBegin) + `.*?` + regexp.QuoteMeta(sectionEnd) + `\n?`),
		regexp.MustCompile(`(?s)\n?<!-- autoskills:section:begin -->.*?<!-- autoskills:section:end -->\n?`),
	}
)

// SectionBudgetBytes caps the managed AGENTS.md section. Context is a finite resource — an
// unbounded section degrades the agent it's meant to help ("Markdown poisoning"). On overflow
// the lowest-confidence skills are demoted to on-demand skill files. Callers set this from
// config; the default matches config.SectionBudgetBytes.
var SectionBudgetBytes = 12000

// Block is one managed skill block parsed from an AGENTS.md section.
type Block struct {
	ID         string
	Group      string
	Confidence float64
	Body       string // includes the "#### Title" heading line
}

// ParseBlocks extracts every temper-managed block from AGENTS.md content (used by the
// gardener and `verify`).
func ParseBlocks(content string) []Block {
	var out []Block
	for _, blockRe := range blockRes {
		for _, m := range blockRe.FindAllStringSubmatch(content, -1) {
			b := Block{ID: m[1], Group: m[2], Body: strings.TrimSpace(m[4])}
			if !knownGroup(b.Group) {
				b.Group = "conventions" // unknown/missing groups must never be silently dropped on rebuild
			}
			if c, err := strconv.ParseFloat(m[3], 64); err == nil {
				b.Confidence = c
			}
			out = append(out, b)
		}
	}
	return out
}

// BlockTitle extracts the human title from a block body's heading line.
func BlockTitle(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if t, ok := strings.CutPrefix(line, "#### "); ok {
			return strings.TrimSpace(t)
		}
		if t, ok := strings.CutPrefix(line, "## "); ok {
			return strings.TrimSpace(t)
		}
	}
	return ""
}

// groups render in this fixed order; a suggestion's signal decides its group.
var groupOrder = []struct{ key, heading string }{
	{"conventions", "Conventions"},
	{"workflows", "Commands & workflows"},
	{"pitfalls", "Pitfalls"},
}

func knownGroup(g string) bool {
	for _, grp := range groupOrder {
		if grp.key == g {
			return true
		}
	}
	return false
}

func groupForSignal(signal string) string {
	switch signal {
	case "rediscovery", "workflow":
		return "workflows"
	case "failure_fix":
		return "pitfalls"
	default: // convention, correction
		return "conventions"
	}
}

// planAgentsBlock computes the managed section and its two bounded secondary effects — skills
// demoted under .cursor when the section overflows, and an import line appended to an existing
// CLAUDE.md. All three are parts of one mutation, so they commit together and roll back together.
func planAgentsBlock(g store.Suggestion, plan Plan) (Mutation, error) {
	path := plan.Path
	root := plan.Root
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return Mutation{}, err
	}
	content := string(existing)

	// collect every existing managed block (inside the section or legacy standalone ones)
	blocks := map[string]Block{}
	var order []string
	for _, b := range ParseBlocks(content) {
		// legacy blocks used h2 titles; normalize to the grouped layout's h4 (prefix-anchored —
		// a bare Replace would corrupt already-normalized "#### " headings, which contain "## ")
		if strings.HasPrefix(b.Body, "## ") {
			b.Body = "#### " + strings.TrimPrefix(b.Body, "## ")
		}
		blocks[b.ID] = b
		order = append(order, b.ID)
	}

	// upsert (or, for an empty body, prune) the target block
	id := g.BlockID
	if id == "" {
		id = g.ID
		// distiller-proposed amendments ("amend: X") carry no BlockID — resolve the target by
		// title so they rewrite the existing block instead of silently duplicating it. The walk
		// follows `order`, not the map: two blocks sharing a title must resolve to the same one on
		// every run, or the same input would produce two different mutations.
		if t, isAmend := strings.CutPrefix(g.Title, "amend: "); isAmend {
			for _, candidate := range order {
				b, ok := blocks[candidate]
				if ok && strings.EqualFold(BlockTitle(b.Body), strings.TrimSpace(t)) {
					id = b.ID
					break
				}
			}
		}
	}
	if strings.TrimSpace(g.Body) == "" {
		delete(blocks, id) // gardener prune
	} else {
		if _, seen := blocks[id]; !seen {
			order = append(order, id)
		}
		// gardener amendments arrive titled "amend: X" for the inbox; the block heading stays clean
		title := strings.TrimPrefix(g.Title, "amend: ")
		blocks[id] = Block{
			ID:         id,
			Group:      groupForSignal(g.Signal),
			Confidence: g.Confidence,
			Body:       fmt.Sprintf("#### %s\n\n%s", title, strings.TrimSpace(g.Body)),
		}
	}

	mut := Mutation{WrittenPath: path}

	// budget enforcement: demote lowest-confidence blocks to on-demand skill files until the
	// section fits. The just-written block is exempt — the human explicitly chose it.
	section := renderSection(blocks, order)
	for len(section) > SectionBudgetBytes && len(blocks) > 1 {
		victim, ok := lowestConfidence(blocks, id)
		if !ok {
			break
		}
		op, notice, err := demotionOp(root, victim)
		if err != nil {
			return Mutation{}, fmt.Errorf("demote %s: %w", victim.ID, err)
		}
		mut.Ops = append(mut.Ops, op)
		mut.Notices = append(mut.Notices, notice)
		delete(blocks, victim.ID)
		section = renderSection(blocks, order)
	}

	// strip the old section and any legacy blocks, then rebuild from scratch
	for _, sectionRe := range sectionRes {
		content = sectionRe.ReplaceAllString(content, "\n")
	}
	for _, blockRe := range blockRes {
		content = blockRe.ReplaceAllString(content, "")
	}
	content = strings.TrimRight(content, "\n")
	if content == "" {
		content = "# AGENTS.md\n"
	}
	content += "\n" + section
	mut.Ops = append(mut.Ops, FileOp{Root: root, Path: path, Content: content})

	op, needed, err := claudeImportOp(root)
	if err != nil {
		return Mutation{}, err
	}
	if needed {
		mut.Ops = append(mut.Ops, op)
	}
	return mut, nil
}

func renderSection(blocks map[string]Block, order []string) string {
	var sb strings.Builder
	sb.WriteString(sectionBegin + "\n")
	sb.WriteString("## Agent skills (temper)\n\n")
	sb.WriteString("Skills distilled from agent sessions and accepted in review. Managed by temper — update via `temper review`, not by hand.\n")
	for _, grp := range groupOrder {
		var members []Block
		for _, id := range order {
			if b, ok := blocks[id]; ok && b.Group == grp.key {
				members = append(members, b)
			}
		}
		if len(members) == 0 {
			continue
		}
		sb.WriteString("\n### " + grp.heading + "\n")
		for _, b := range members {
			fmt.Fprintf(&sb, "\n<!-- temper:begin id=%s group=%s conf=%.2f -->\n%s\n<!-- temper:end id=%s -->\n", b.ID, b.Group, b.Confidence, b.Body, b.ID)
		}
	}
	sb.WriteString(sectionEnd + "\n")
	return sb.String()
}

// lowestConfidence picks the block to demote. Confidence decides; block id breaks a tie. Without
// that tie-break the choice came from Go's randomized map iteration, so two runs of the same
// planning on the same repository could evict different skills — and a mutation that is not a
// function of its input cannot be reviewed before it is applied, nor replayed after a crash.
func lowestConfidence(blocks map[string]Block, exemptID string) (Block, bool) {
	var victim Block
	found := false
	for _, b := range blocks {
		if b.ID == exemptID {
			continue
		}
		if !found || b.Confidence < victim.Confidence || (b.Confidence == victim.Confidence && b.ID < victim.ID) {
			victim, found = b, true
		}
	}
	return victim, found
}

// demotionOp moves an over-budget block to an on-demand skill file so it stays available without
// occupying always-on context. The destination is confined here and again at mutation time.
func demotionOp(repoRoot string, b Block) (FileOp, string, error) {
	title := BlockTitle(b.Body)
	if title == "" {
		title = b.ID
	}
	dir := filepath.Join(repoRoot, ".cursor", "skills", "temper-"+slug(title))
	path := filepath.Join(dir, "SKILL.md")
	if err := confine(repoRoot, path); err != nil {
		return FileOp{}, "", err
	}
	body := strings.TrimSpace(strings.TrimPrefix(b.Body, "#### "+title))
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n%s\n\n<!-- temper:demoted id=%s reason=section-budget -->\n", slug(title), yamlEscape(title), body, b.ID)
	return FileOp{Root: repoRoot, Path: path, Content: content},
		fmt.Sprintf("  budget: demoted %q to %s\n", title, path), nil
}

// removeUnjournaled deletes an accepted suggestion's artifact. Like writeUnjournaled it is atomic
// but not durable, unexported, and has no production caller: Undo is the only way in, and it
// compensates the journaled acceptance instead of recomputing a deletion.
func removeUnjournaled(g store.Suggestion) error {
	mut, err := BuildRemoval(g)
	if err != nil {
		return err
	}
	if err := capture(&mut); err != nil {
		return err
	}
	if err := applyOps(&mut); err != nil {
		if uErr := unwind(&mut); uErr != nil {
			return fmt.Errorf("%w; %v", err, uErr)
		}
		return err
	}
	return nil
}

// BuildRemoval computes the mutation that undoes an accepted suggestion. AGENTS.md blocks are
// pruned from the managed section; standalone files are deleted.
//
// A stored path is data, not authority — and that holds for the SHAPE of the removal as much as
// for its target. Which of the two shapes applies is decided by the plan recomputed from the
// suggestion, never by what written_path happens to be named: routing on the stored basename let
// a tampered row aim a `skill` suggestion at the repository's AGENTS.md, a file that suggestion
// never wrote. A standalone removal then refuses anything that is not exactly the artifact this
// suggestion produces, so a stale or tampered path cannot aim a deletion at an unrelated file
// either.
func BuildRemoval(g store.Suggestion) (Mutation, error) {
	if g.WrittenPath == "" {
		return Mutation{}, nil
	}
	plan, err := BuildPlan(g)
	if err != nil {
		return Mutation{}, err
	}
	if plan.Kind == KindAgentsBlock {
		gg := g
		gg.Body = "" // empty body = prune
		prunePlan, err := BuildPlan(gg)
		if err != nil {
			return Mutation{}, err
		}
		return planAgentsBlock(gg, prunePlan)
	}
	storedPath := filepath.Clean(g.WrittenPath)
	targetPath := filepath.Clean(plan.Path)
	legacyPath := legacyArtifactPath(plan)
	switch storedPath {
	case targetPath:
	case legacyPath:
		targetPath = legacyPath
		// The home migration moves machine skills with the directory while the historical database
		// row still names ~/.autoskills. Prefer the migrated location when it exists.
		if plan.Kind == KindMachineSkill {
			if _, err := os.Lstat(plan.Path); err == nil {
				targetPath = plan.Path
			}
		}
	default:
		return Mutation{}, fmt.Errorf("writer: refusing to remove %q: this suggestion's artifact is %q", clip(g.WrittenPath), clip(plan.Path))
	}
	if err := confine(plan.Root, targetPath); err != nil {
		return Mutation{}, err
	}
	mut := Mutation{Ops: []FileOp{{Root: plan.Root, Path: targetPath, Remove: true}}}
	dir := filepath.Dir(targetPath)
	// older versions emitted an executable run.sh next to a skill (removed in HOK-539); undo must
	// still clean one up when it exists, but only inside directories we own (temper- prefix)
	if strings.HasPrefix(filepath.Base(dir), "temper-") || strings.HasPrefix(filepath.Base(dir), "autoskills-") {
		mut.Ops = append(mut.Ops, FileOp{Root: plan.Root, Path: filepath.Join(dir, "run.sh"), Remove: true})
	}
	// the now-empty `temper-<slug>` directory is left where it is. Removing a directory needs
	// proof that this operation created it and that nothing else has since been put inside, and a
	// manifest written before the mutation cannot carry the second half across a crash. An empty
	// directory is untidy; deleting one the user had put something in is not recoverable.
	return mut, nil
}

func legacyArtifactPath(plan Plan) string {
	switch plan.Kind {
	case KindMachineSkill:
		return filepath.Join(filepath.Dir(filepath.Dir(plan.Root)), ".autoskills", "skills", filepath.Base(plan.Path))
	case KindCursorRule:
		return filepath.Join(filepath.Dir(plan.Path), strings.Replace(filepath.Base(plan.Path), "temper-", "autoskills-", 1))
	case KindRepoSkill:
		dir := filepath.Dir(plan.Path)
		legacyDir := filepath.Join(filepath.Dir(dir), strings.Replace(filepath.Base(dir), "temper-", "autoskills-", 1))
		return filepath.Join(legacyDir, filepath.Base(plan.Path))
	default:
		return plan.Path
	}
}

// claudeImportOp makes an existing CLAUDE.md pick up AGENTS.md content via the official @import
// syntax. It reports no operation unless CLAUDE.md already exists and lacks any AGENTS.md
// reference — and refuses outright when the file is a link out of the repository, before the
// AGENTS.md write it accompanies is planned.
func claudeImportOp(repoRoot string) (FileOp, bool, error) {
	path := filepath.Join(repoRoot, "CLAUDE.md")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return FileOp{}, false, nil
	} else if err != nil {
		return FileOp{}, false, err
	}
	if err := confine(repoRoot, path); err != nil {
		return FileOp{}, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileOp{}, false, nil
		}
		return FileOp{}, false, err
	}
	if strings.Contains(string(raw), "AGENTS.md") {
		return FileOp{}, false, nil
	}
	content := string(raw)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += "\n@AGENTS.md\n"
	return FileOp{Root: repoRoot, Path: path, Content: content}, true, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.ToLower(s)
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = s[:48]
		s = strings.Trim(s, "-")
	}
	if s == "" {
		s = "skill"
	}
	return s
}

func yamlEscape(s string) string {
	if strings.ContainsAny(s, ":#{}[],&*?|->!%@`\"'") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}
