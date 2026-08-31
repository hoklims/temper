package distill

import (
	"regexp"
	"strings"

	"github.com/hoklims/temper/internal/canon"
)

// The pre-filter is the cheap, deterministic stage that runs BEFORE any LLM call — the same
// idea as a tracker's burst-edit/large-paste heuristics: dumb signals decide what is even worth
// the expensive pass. Most sessions carry no skill, so cheaply proving "this transcript shows
// none of the five signal types" lets us skip the model entirely, cutting tokens and cost. The
// gate deliberately errs toward inclusion: it only skips when NOTHING matches, and adding markers
// can only make it more permissive, so a real skill is not gated out by this stage.

var (
	// correction / convention markers, authored by the user steering the agent.
	correctionMarkers = []string{
		"no, actually", "no actually", "that's wrong", "thats wrong", "that is wrong",
		"don't ", "do not ", "instead of", "instead,", "not like that", "stop ",
		"i said", "i told you", "you should", "why did you", "revert", "undo that",
		"incorrect", "that's not", "thats not", "we don't", "we dont",
	}
	conventionMarkers = []string{
		"we use", "we always", "we never", "always use", "never use", "make sure",
		"prefer ", "by convention", "in this repo", "in this project", "our convention",
		"the convention", "should always", "should never", "must always", "must never",
	}
	// failure markers indicate an error that a later turn likely fixed.
	failureMarkers = []string{
		"error", "failed", "failure", "traceback", "exception", "panic:", "segfault",
		"cannot find", "not found", "no such file", "syntaxerror", "undefined", "cannot read",
		"is not defined", "command not found", "permission denied", "fatal:",
	}
	// workflow markers signal a stated multi-step procedure (the 5th signal type).
	workflowMarkers = []string{
		"step 1", "step 2", "step one", "step two", "first,", "first you", "first we",
		"then run", "then we", "after that", "afterwards", "finally,", "finally we",
		"the workflow", "the procedure", "the process is", "the steps are", "to deploy",
		"to release", "to set up", "to set this up", "to run the",
	}
	// command lines whose repetition signals the agent re-deriving a stable build/test invocation.
	commandRe = regexp.MustCompile(`(?m)^[\s$>]*((?:npm|pnpm|yarn|bun|go|cargo|pytest|python|python3|make|docker|docker-compose|kubectl|terraform|rails|bundle|gradle|mvn|dotnet|composer|pip|pip3)\b[^\n|&;]{0,80})`)
)

// HasSignal cheaply scans a session for any of the five distill signals. It returns the kinds it
// found and whether the session is worth sending to the model. It is deliberately conservative:
// it only reports false (skip) when the transcript shows NONE of the markers, so a real skill is
// never gated out by this stage.
func HasSignal(sess *canon.Session) (kinds []string, worth bool) {
	var userText, allText strings.Builder
	for _, t := range sess.Turns {
		lower := strings.ToLower(t.Text)
		allText.WriteString(lower)
		allText.WriteByte('\n')
		if t.Role == canon.RoleUser {
			userText.WriteString(lower)
			userText.WriteByte('\n')
		}
	}
	user := userText.String()
	all := allText.String()

	seen := map[string]bool{}
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}

	repeatedCmd, distinctCmds := commandStats(all)

	if containsAny(user, correctionMarkers) {
		add("correction")
	}
	if containsAny(all, conventionMarkers) {
		add("convention")
	}
	if containsAny(all, failureMarkers) {
		add("failure_fix")
	}
	if repeatedCmd {
		add("rediscovery")
	}
	// workflow: an explicitly narrated multi-step procedure, or 3+ distinct build/test/deploy
	// commands run in sequence (the shape of a repeatable how-to).
	if containsAny(all, workflowMarkers) || distinctCmds >= 3 {
		add("workflow")
	}

	return kinds, len(kinds) > 0
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// commandStats counts build/test-style command lines: repeated reports whether any single
// command appears 2+ times (rediscovery signature), distinct is the number of unique commands
// (3+ distinct commands in one session is the shape of a multi-step workflow).
func commandStats(text string) (repeated bool, distinct int) {
	counts := map[string]int{}
	for _, m := range commandRe.FindAllStringSubmatch(text, -1) {
		cmd := strings.Join(strings.Fields(m[1]), " ") // normalize whitespace
		if len(cmd) < 3 {
			continue
		}
		counts[cmd]++
	}
	for _, c := range counts {
		if c >= 2 {
			repeated = true
			break
		}
	}
	return repeated, len(counts)
}
