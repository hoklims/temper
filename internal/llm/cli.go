package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/hoklims/temper/internal/outbound"
)

var (
	ErrCLIUnavailable   = errors.New("llm: CLI unavailable")
	ErrNotAuthenticated = errors.New("llm: CLI not authenticated")
	ErrEmptyOutput      = errors.New("llm: empty provider output")
	ErrInvalidOutput    = errors.New("llm: invalid structured output")
	ErrOutputTooLarge   = errors.New("llm: provider output too large")
)

const (
	maxCLIOutputBytes = 8 << 20
	maxCLIStderrBytes = 1 << 20
)

type cliProvider struct {
	kind    string
	command []string
	model   string
	timeout time.Duration
}

type claudeEnvelope struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
}

func NewCodex(model string) (Provider, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("%w: codex executable not found", ErrCLIUnavailable)
	}
	return newCodexProvider([]string{path}, model, 180*time.Second), nil
}

func NewClaude(model string) (Provider, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return nil, fmt.Errorf("%w: claude executable not found", ErrCLIUnavailable)
	}
	return newClaudeProvider([]string{path}, model, 180*time.Second), nil
}

func newCodexProvider(command []string, model string, timeout time.Duration) Provider {
	return &cliProvider{kind: "codex", command: command, model: model, timeout: timeout}
}

func newClaudeProvider(command []string, model string, timeout time.Duration) Provider {
	return &cliProvider{kind: "claude", command: command, model: model, timeout: timeout}
}

func (p *cliProvider) Generate(ctx context.Context, payload outbound.Payload) (output string, resultErr error) {
	dir, err := neutralWorkingDirectory(payload.ExcludedRoots())
	if err != nil {
		return "", err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("llm: remove neutral working directory: %w", cleanupErr))
		}
	}()

	runCtx := ctx
	cancel := func() {}
	if p.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()

	switch p.kind {
	case "codex":
		return p.generateCodex(runCtx, dir, payload)
	case "claude":
		return p.generateClaude(runCtx, dir, payload)
	default:
		return "", fmt.Errorf("llm: unknown CLI provider %q", p.kind)
	}
}

func (p *cliProvider) generateCodex(ctx context.Context, dir string, payload outbound.Payload) (output string, resultErr error) {
	if payload.OutputSchema() == "" {
		return "", fmt.Errorf("%w from codex: missing output schema", ErrInvalidOutput)
	}
	authPath, err := codexAuthPath()
	if err != nil {
		return "", err
	}
	codexHome, err := neutralWorkingDirectory(append(payload.ExcludedRoots(), dir))
	if err != nil {
		return "", fmt.Errorf("llm: create neutral Codex home: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(codexHome); cleanupErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("llm: remove neutral Codex home: %w", cleanupErr))
		}
	}()
	if err := stageCodexAuth(authPath, filepath.Join(codexHome, "auth.json")); err != nil {
		return "", fmt.Errorf("llm: stage Codex authentication in neutral home: %w", err)
	}
	schemaPath := filepath.Join(dir, "output-schema.json")
	outputPath := filepath.Join(dir, "last-message.json")
	if err := os.WriteFile(schemaPath, []byte(payload.OutputSchema()), 0o600); err != nil {
		return "", fmt.Errorf("llm: write Codex output schema: %w", err)
	}
	args := []string{"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--strict-config", "-c", "project_doc_max_bytes=0", "-c", "mcp_servers={}", "--enable", "skip_host_skill_discovery", "--disable", "shell_tool", "--disable", "unified_exec", "--disable", "shell_snapshot", "--disable", "code_mode", "--disable", "code_mode_host", "--disable", "code_mode_only", "--disable", "multi_agent", "--disable", "browser_use", "--disable", "browser_use_external", "--disable", "browser_use_full_cdp_access", "--disable", "in_app_browser", "--disable", "computer_use", "--disable", "apps", "--disable", "plugins", "--disable", "plugin_sharing", "--disable", "remote_plugin", "--disable", "hooks", "--disable", "skill_search", "--disable", "skill_mcp_dependency_install", "--disable", "tool_suggest", "--disable", "tool_call_mcp_elicitation", "--disable", "auth_elicitation", "--disable", "goals", "--disable", "workspace_dependencies", "--disable", "in_app_chat", "--disable", "in_app_local_automation", "--disable", "in_app_updates", "--disable", "image_generation", "--disable", "view_image", "--skip-git-repo-check", "--sandbox", "read-only", "--color", "never"}
	if p.model != "" {
		args = append(args, "--model", p.model)
	}
	args = append(args, "--cd", dir, "--output-schema", schemaPath, "--output-last-message", outputPath, "-")
	stdin := "SYSTEM INSTRUCTIONS:\n" + payload.System() + "\n\nUSER MESSAGE:\n" + payload.User()
	if _, err := p.run(ctx, dir, args, stdin, stdin, map[string]string{"CODEX_HOME": codexHome}); err != nil {
		return "", err
	}
	raw, err := readBoundedFile(outputPath, maxCLIOutputBytes)
	if err != nil {
		return "", fmt.Errorf("llm: codex final output: %w", err)
	}
	return validateStructuredOutput("codex", raw)
}

func (p *cliProvider) generateClaude(ctx context.Context, dir string, payload outbound.Payload) (string, error) {
	if payload.OutputSchema() == "" {
		return "", fmt.Errorf("%w from claude: missing output schema", ErrInvalidOutput)
	}
	systemPromptPath := filepath.Join(dir, "system-prompt.txt")
	if err := os.WriteFile(systemPromptPath, []byte(payload.System()), 0o600); err != nil {
		return "", fmt.Errorf("llm: write Claude system prompt: %w", err)
	}
	args := []string{"-p", "--safe-mode", "--disable-slash-commands", "--no-session-persistence", "--tools", "", "--permission-mode", "dontAsk", "--output-format", "json", "--json-schema", payload.OutputSchema()}
	if p.model != "" {
		args = append(args, "--model", p.model)
	}
	args = append(args, "--system-prompt-file", systemPromptPath)
	sensitiveInput := payload.System() + "\n" + payload.User()
	stdout, err := p.run(ctx, dir, args, payload.User(), sensitiveInput, nil)
	if err != nil {
		return "", err
	}
	var envelope claudeEnvelope
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		return "", fmt.Errorf("%w from claude: %v", ErrInvalidOutput, err)
	}
	if envelope.IsError || envelope.Type != "result" || envelope.Subtype != "success" {
		return "", fmt.Errorf("llm: claude returned %s/%s", envelope.Type, envelope.Subtype)
	}
	if len(envelope.StructuredOutput) > 0 && string(envelope.StructuredOutput) != "null" {
		return validateStructuredOutput("claude", envelope.StructuredOutput)
	}
	return validateStructuredOutput("claude", []byte(envelope.Result))
}

func (p *cliProvider) run(ctx context.Context, dir string, args []string, stdin, sensitiveInput string, environment map[string]string) ([]byte, error) {
	commandArgs := append(append([]string{}, p.command[1:]...), args...)
	cmd := exec.CommandContext(ctx, p.command[0], commandArgs...)
	isolateCommandProcess(cmd)
	cmd.WaitDelay = time.Second
	cmd.Dir = dir
	cmd.Env = providerEnvironment(p.kind, environment)
	cmd.Stdin = strings.NewReader(stdin)
	stdout := newBoundedBuffer(maxCLIOutputBytes)
	stderr := newBoundedBuffer(maxCLIStderrBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("llm: %s: %w", p.kind, ctx.Err())
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("%w from %s", ErrOutputTooLarge, p.kind)
	}
	if err != nil {
		sanitizedStderr := outbound.Sanitize(stderr.String())
		if cliFailureLooksUnauthenticated(p.kind, sanitizedStderr, stdout.Bytes(), sensitiveInput) {
			return nil, fmt.Errorf("%w: %s", ErrNotAuthenticated, p.kind)
		}
		detail := safeCLIDetail(sanitizedStderr, sensitiveInput)
		if detail == "" {
			return nil, fmt.Errorf("llm: %s exited non-zero: %w", p.kind, err)
		}
		return nil, fmt.Errorf("llm: %s exited non-zero: %w: %s", p.kind, err, detail)
	}
	return stdout.Bytes(), nil
}

func codexAuthPath() (string, error) {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: resolve Codex home: %v", ErrNotAuthenticated, err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	authPath, err := filepath.Abs(filepath.Join(codexHome, "auth.json"))
	if err != nil {
		return "", fmt.Errorf("%w: resolve Codex auth.json: %v", ErrNotAuthenticated, err)
	}
	info, err := os.Stat(authPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%w: codex auth.json not found; run codex login", ErrNotAuthenticated)
		}
		return "", fmt.Errorf("%w: inspect Codex auth.json: %v", ErrNotAuthenticated, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: Codex auth.json is not a regular file", ErrNotAuthenticated)
	}
	return authPath, nil
}

func stageCodexAuth(source, destination string) error {
	return stageCodexAuthWithLink(source, destination, os.Symlink, runtime.GOOS != "windows")
}

func stageCodexAuthWithLink(source, destination string, link func(string, string) error, allowCopy bool) (resultErr error) {
	linkErr := link(source, destination)
	if linkErr == nil {
		return nil
	}
	if !allowCopy {
		return fmt.Errorf("Codex auth.json requires symbolic-link support on Windows: %w", linkErr)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := output.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
		}
		if resultErr != nil {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	return output.Chmod(0o600)
}

func providerEnvironment(kind string, overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	replacedNames := make(map[string]struct{}, len(overrides))
	for name := range overrides {
		replacedNames[strings.ToUpper(name)] = struct{}{}
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		_, replaced := replacedNames[strings.ToUpper(name)]
		if replaced || providerEnvironmentVariableBlocked(kind, name) {
			continue
		}
		environment = append(environment, entry)
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment
}

func providerEnvironmentVariableBlocked(kind, name string) bool {
	name = strings.ToUpper(name)
	if kind == "claude" && name == "CLAUDE_CONFIG_DIR" {
		return false
	}
	// Temper's own configuration is parent-only. TEMPER_API_KEY is a provider credential for
	// the HTTP path and has no business in a CLI child; the rest (provider, endpoint, model) would
	// let a subprocess read — or be steered by — this process's configuration. The whole prefix is
	// removed rather than a list of known names, because a name added later would leak silently.
	if strings.HasPrefix(name, "TEMPER_") || strings.HasPrefix(name, "AUTOSKILLS_") {
		return true
	}
	if name == "RUST_LOG" || name == "RUST_BACKTRACE" || strings.HasPrefix(name, "OTEL_") || strings.HasPrefix(name, "CODEX_") || strings.HasPrefix(name, "OPENAI_") || strings.HasPrefix(name, "CLAUDE_") || strings.HasPrefix(name, "ANTHROPIC_") {
		return true
	}
	switch name {
	case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "AZURE_TENANT_ID":
		return true
	}
	return false
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.overflow = true
		return originalLength, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		b.overflow = true
	}
	_, _ = b.buffer.Write(value)
	return originalLength, nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }
func (b *boundedBuffer) Bytes() []byte  { return b.buffer.Bytes() }

func readBoundedFile(path string, limit int64) ([]byte, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !pathInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: final output is not a regular file", ErrInvalidOutput)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(pathInfo, openedInfo) {
		return nil, fmt.Errorf("%w: final output changed while opening", ErrInvalidOutput)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, ErrOutputTooLarge
	}
	return raw, nil
}

func neutralWorkingDirectory(excludedRoots []string) (string, error) {
	callerDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("llm: resolve caller working directory: %w", err)
	}
	createdDir, err := os.MkdirTemp("", "temper-llm-")
	if err != nil {
		return "", fmt.Errorf("llm: create neutral working directory: %w", err)
	}
	dir, err := filepath.EvalSymlinks(createdDir)
	if err != nil {
		_ = os.RemoveAll(createdDir)
		return "", fmt.Errorf("llm: resolve neutral working directory: %w", err)
	}
	homeBoundary := ""
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		if resolvedHome, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
			homeBoundary = resolvedHome
		}
	}
	roots := append([]string{}, excludedRoots...)
	if filepath.Dir(callerDir) != callerDir {
		roots = append(roots, callerDir)
	}
	for _, root := range roots {
		resolvedRoot, resolveErr := filepath.EvalSymlinks(root)
		if resolveErr != nil {
			if errors.Is(resolveErr, os.ErrNotExist) {
				if pathWithin(root, dir) {
					_ = os.RemoveAll(dir)
					return "", fmt.Errorf("llm: neutral working directory must be outside excluded roots")
				}
				continue
			}
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("llm: resolve excluded root: %w", resolveErr)
		}
		inside, insideErr := pathWithinFilesystem(resolvedRoot, dir)
		if insideErr != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("llm: compare neutral working directory with excluded root: %w", insideErr)
		}
		if inside {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("llm: neutral working directory must be outside excluded roots")
		}
	}
	for current := dir; ; current = filepath.Dir(current) {
		if homeBoundary != "" && samePath(current, homeBoundary) {
			break
		}
		for _, name := range []string{"AGENTS.md", "AGENTS.override.md", "CLAUDE.md", ".git", ".codex", ".claude"} {
			_, statErr := os.Stat(filepath.Join(current, name))
			switch {
			case statErr == nil:
				_ = os.RemoveAll(dir)
				return "", fmt.Errorf("llm: neutral working directory has provider configuration or instructions in an ancestor")
			case !errors.Is(statErr, os.ErrNotExist):
				_ = os.RemoveAll(dir)
				return "", fmt.Errorf("llm: inspect neutral working directory ancestry: %w", statErr)
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return dir, nil
}

func samePath(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func pathWithinFilesystem(root, target string) (bool, error) {
	if pathWithin(root, target) {
		return true, nil
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false, err
	}
	for current := target; ; current = filepath.Dir(current) {
		currentInfo, err := os.Stat(current)
		if err != nil {
			return false, err
		}
		if os.SameFile(rootInfo, currentInfo) {
			return true, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
	}
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func safeCLIDetail(stderr, prompt string) string {
	lines := strings.Split(stderr, "\n")
	for _, line := range lines {
		if message, ok := jsonMessage(line); ok {
			message = limitRunes(message, 500)
			if !detailContainsPromptData(message, prompt) {
				return message
			}
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := limitRunes(strings.TrimSpace(lines[i]), 500)
		lower := strings.ToLower(trimmed)
		if (strings.Contains(lower, "error") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "authentication") || strings.Contains(lower, "not logged in")) && !detailContainsPromptData(trimmed, prompt) {
			return trimmed
		}
	}
	return ""
}

func cliFailureLooksUnauthenticated(kind, stderr string, stdout []byte, prompt string) bool {
	if containsUnauthenticatedDiagnostic(stderr, prompt) {
		return true
	}
	if kind != "claude" {
		return false
	}
	var envelope claudeEnvelope
	if json.Unmarshal(stdout, &envelope) != nil || envelope.Type != "result" || !envelope.IsError {
		return false
	}
	return containsUnauthenticatedDiagnostic(outbound.Sanitize(envelope.Result), prompt)
}

func containsUnauthenticatedDiagnostic(output, prompt string) bool {
	for _, line := range strings.Split(output, "\n") {
		candidate := strings.TrimSpace(line)
		if message, ok := jsonMessage(line); ok {
			candidate = message
		}
		if looksUnauthenticated(candidate) && !promptContainsDiagnostic(prompt, candidate) {
			return true
		}
	}
	return false
}

func jsonMessage(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, `"message":`) {
		return "", false
	}
	encoded := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(trimmed, `"message":`)), ",")
	var message string
	if json.Unmarshal([]byte(encoded), &message) != nil {
		return "", false
	}
	return message, true
}

func promptContainsDiagnostic(prompt, diagnostic string) bool {
	diagnostic = strings.TrimSpace(diagnostic)
	return diagnostic != "" && strings.Contains(strings.ToLower(prompt), strings.ToLower(diagnostic))
}

func detailContainsPromptData(detail, prompt string) bool {
	promptLower := strings.ToLower(prompt)
	promptTokens := make(map[string]struct{})
	for _, token := range textTokens(promptLower) {
		promptTokens[token] = struct{}{}
	}
	for _, token := range textTokens(detail) {
		if _, present := promptTokens[token]; present {
			return true
		}
		if len([]rune(token)) >= 4 && strings.Contains(promptLower, token) {
			return true
		}
	}
	detailLower := strings.ToLower(detail)
	for token := range promptTokens {
		if len([]rune(token)) >= 4 && strings.Contains(detailLower, token) {
			return true
		}
	}
	return false
}

func textTokens(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(character rune) bool {
		return !unicode.IsLetter(character) && !unicode.IsNumber(character)
	})
}

func limitRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func looksUnauthenticated(detail string) bool {
	lower := strings.ToLower(detail)
	return strings.Contains(lower, "authentication required") || strings.Contains(lower, "not logged in") || strings.Contains(lower, "unauthorized")
}

func validateStructuredOutput(kind string, raw []byte) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", fmt.Errorf("%w from %s", ErrEmptyOutput, kind)
	}
	if !json.Valid(trimmed) {
		return "", fmt.Errorf("%w from %s", ErrInvalidOutput, kind)
	}
	return string(trimmed), nil
}
