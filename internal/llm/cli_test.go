package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hoklims/temper/internal/outbound"
)

const testOutputSchema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`

type helperInvocation struct {
	Args             []string `json:"args"`
	Dir              string   `json:"dir"`
	Stdin            string   `json:"stdin"`
	CodexHome        string   `json:"codex_home,omitempty"`
	CodexAuthTarget  string   `json:"codex_auth_target,omitempty"`
	CodexAuthRegular bool     `json:"codex_auth_regular,omitempty"`
	CodexHomeMode    uint32   `json:"codex_home_mode,omitempty"`
	SystemPrompt     string   `json:"system_prompt,omitempty"`
	EnvironmentLeaks []string `json:"environment_leaks,omitempty"`
	// ProductNames is every TEMPER_* or legacy AUTOSKILLS_* variable the child can see, by name.
	// A named list
	// would only prove the absence of names someone remembered to write down.
	ProductNames []string `json:"product_names,omitempty"`
	// PathPresent proves the child kept the environment it needs to run: an empty environment
	// would pass every absence assertion above while breaking the provider.
	PathPresent bool `json:"path_present,omitempty"`
}

func TestCLIHelper(t *testing.T) {
	if os.Getenv("TEST_CLI_HELPER_BEHAVIOR") == "" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 {
		os.Exit(90)
	}
	args := os.Args[separator+1:]
	stdin, _ := io.ReadAll(os.Stdin)
	dir, _ := os.Getwd()
	codexHome := os.Getenv("CODEX_HOME")
	var codexAuthTarget string
	var codexAuthRegular bool
	var codexHomeMode uint32
	if codexHome != "" {
		codexAuthTarget, _ = os.Readlink(filepath.Join(codexHome, "auth.json"))
		if info, err := os.Lstat(filepath.Join(codexHome, "auth.json")); err == nil {
			codexAuthRegular = info.Mode().IsRegular()
		}
		if info, err := os.Stat(codexHome); err == nil {
			codexHomeMode = uint32(info.Mode().Perm())
		}
	}
	var systemPrompt string
	if systemPromptPath := argumentValue(args, "--system-prompt-file"); systemPromptPath != "" {
		raw, _ := os.ReadFile(systemPromptPath)
		systemPrompt = string(raw)
	}
	var environmentLeaks []string
	for _, name := range []string{"OPENAI_API_KEY", "OpenAI_API_KEY", "CODEX_ACCESS_TOKEN", "ANTHROPIC_API_KEY", "Anthropic_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "AWS_SECRET_ACCESS_KEY", "RUST_LOG", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		if _, present := os.LookupEnv(name); present {
			environmentLeaks = append(environmentLeaks, name)
		}
	}
	var productNames []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "TEMPER_") || strings.HasPrefix(upper, "AUTOSKILLS_") {
			productNames = append(productNames, name)
		}
	}
	_, pathPresent := os.LookupEnv("PATH")
	if !pathPresent {
		_, pathPresent = os.LookupEnv("Path") // Windows preserves the case it was given
	}
	invocation, _ := json.Marshal(helperInvocation{Args: args, Dir: dir, Stdin: string(stdin), CodexHome: codexHome, CodexAuthTarget: codexAuthTarget, CodexAuthRegular: codexAuthRegular, CodexHomeMode: codexHomeMode, SystemPrompt: systemPrompt, EnvironmentLeaks: environmentLeaks, ProductNames: productNames, PathPresent: pathPresent})
	switch os.Getenv("TEST_CLI_HELPER_BEHAVIOR") {
	case "exit":
		_, _ = fmt.Fprint(os.Stderr, "authentication required")
		os.Exit(7)
	case "not-logged-in":
		_, _ = fmt.Fprint(os.Stderr, "Not logged in; run /login")
		os.Exit(7)
	case "authentication-required":
		_, _ = fmt.Fprint(os.Stderr, "authentication required")
		os.Exit(7)
	case "claude-auth-json":
		_, _ = fmt.Fprint(os.Stdout, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Not logged in · Please run /login"}`)
		os.Exit(7)
	case "claude-prompt-auth-json":
		_, _ = fmt.Fprint(os.Stdout, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"authentication required in private prompt"}`)
		os.Exit(8)
	case "request-error":
		_, _ = fmt.Fprint(os.Stderr, "SYSTEM INSTRUCTIONS:\nsystem instruction\nUSER MESSAGE:\nauthentication required in private prompt\nERROR: {\n  \"error\": {\n    \"message\": \"schema rejected\"\n  }\n}\n")
		os.Exit(8)
	case "prompt-error":
		_, _ = fmt.Fprint(os.Stderr, "Error: private-customer-project-codename")
		os.Exit(8)
	case "prompt-json-error":
		_, _ = fmt.Fprint(os.Stderr, `"message": "private-customer-project-codename"`)
		os.Exit(8)
	case "short-prompt-error":
		_, _ = fmt.Fprint(os.Stderr, "Error: x7")
		os.Exit(8)
	case "system-prompt-error":
		_, _ = fmt.Fprint(os.Stderr, "Error: system-private-customer-codename")
		os.Exit(8)
	case "prompt-partial-token-error":
		_, _ = fmt.Fprint(os.Stderr, "Error: customerproject")
		os.Exit(8)
	case "large":
		large := strings.Repeat("x", maxCLIOutputBytes+1)
		if output := argumentValue(args, "--output-last-message"); output != "" {
			_ = os.WriteFile(output, []byte(`{"value":"`+large+`"}`), 0o600)
			return
		}
		_, _ = fmt.Fprint(os.Stdout, large)
		os.Exit(0)
	case "empty":
		if output := argumentValue(args, "--output-last-message"); output != "" {
			_ = os.WriteFile(output, nil, 0o600)
		}
		_, _ = fmt.Fprint(os.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":""}`)
		os.Exit(0)
	case "invalid":
		if output := argumentValue(args, "--output-last-message"); output != "" {
			_ = os.WriteFile(output, []byte("not json"), 0o600)
		}
		_, _ = fmt.Fprint(os.Stdout, `{"type":"result","subtype":"success","is_error":false,"result":"not json"}`)
		os.Exit(0)
	case "symlink-output":
		if output := argumentValue(args, "--output-last-message"); output != "" {
			target := filepath.Join(filepath.Dir(output), "redirected-output.json")
			_ = os.WriteFile(target, []byte(`{"ok":true}`), 0o600)
			_ = os.Symlink(target, output)
		}
		os.Exit(0)
	case "sleep":
		time.Sleep(5 * time.Second)
	case "spawn-child":
		child := exec.Command(os.Args[0], "-test.run=TestCLIHelper", "--")
		child.Env = append(os.Environ(), "TEST_CLI_HELPER_BEHAVIOR=child-write")
		child.Stdout = io.Discard
		child.Stderr = io.Discard
		if child.Start() != nil {
			os.Exit(91)
		}
		// announce that the descendant exists, so the test cancels a real process group instead of
		// racing a helper that was killed before it ever spawned anything
		_ = os.WriteFile(os.Getenv("TEST_CLI_HELPER_CHILD_MARKER")+".spawned", []byte("spawned"), 0o600)
		time.Sleep(5 * time.Second)
	case "child-write":
		time.Sleep(2 * time.Second)
		_ = os.WriteFile(os.Getenv("TEST_CLI_HELPER_CHILD_MARKER"), []byte("survived"), 0o600)
		os.Exit(0)
	default:
		if output := argumentValue(args, "--output-last-message"); output != "" {
			_ = os.WriteFile(output, invocation, 0o600)
			return
		}
		envelope, _ := json.Marshal(map[string]any{
			"type": "result", "subtype": "success", "is_error": false, "result": string(invocation),
		})
		_, _ = os.Stdout.Write(envelope)
		os.Exit(0)
	}
}

func argumentValue(args []string, name string) string {
	for i := range args {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func helperCommand() []string {
	return []string{os.Args[0], "-test.run=TestCLIHelper", "--"}
}

// helperTimeout is the deadline for the tests that expect the helper to RUN. Every one of them
// starts a second copy of this test binary, and under `-race` that copy is instrumented too — a
// one-second budget turns a functional assertion into a latency measurement, and the test then
// reports a timeout instead of what it was written to check. The deadline-specific tests below
// stay deliberately short, because there the deadline *is* the subject.
const helperTimeout = 60 * time.Second

func newTestCodexProvider(t *testing.T, model string, timeout time.Duration) Provider {
	t.Helper()
	codexHome := t.TempDir()
	authPath := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"tokens":"test-only"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	return newCodexProvider(helperCommand(), model, timeout)
}

func preparedPayload(t *testing.T) outbound.Payload {
	t.Helper()
	var b outbound.Builder
	b.Static("USER DATA: ").Data("token sk-ant-api03-eeeeeeeeeeeeeeeeeeeeeeee", 0)
	p, err := b.BuildWithOutputSchema("system instruction", testOutputSchema)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func decodeInvocation(t *testing.T, output string) helperInvocation {
	t.Helper()
	var invocation helperInvocation
	if err := json.Unmarshal([]byte(output), &invocation); err != nil {
		t.Fatalf("decode invocation: %v\n%s", err, output)
	}
	return invocation
}

func TestCodexProviderInvocationIsEphemeralAndNeutral(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	t.Setenv("OPENAI_API_KEY", "must-not-reach-codex")
	t.Setenv("OpenAI_API_KEY", "must-not-reach-codex")
	t.Setenv("CODEX_ACCESS_TOKEN", "must-not-reach-codex")
	t.Setenv("Codex_Home", "must-not-reach-codex")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-codex")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "must-not-reach-codex")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-reach-codex")
	t.Setenv("RUST_LOG", "trace")
	p := newTestCodexProvider(t, "gpt-test", helperTimeout)
	payload := preparedPayload(t)
	originalDir, _ := os.Getwd()
	out, err := p.Generate(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeInvocation(t, out)
	if pathWithin(originalDir, got.Dir) || !strings.Contains(filepath.Base(got.Dir), "temper-llm-") {
		t.Fatalf("working directory is not neutral: %q", got.Dir)
	}
	wantPrefix := []string{"exec", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--strict-config", "-c", "project_doc_max_bytes=0", "-c", "mcp_servers={}", "--enable", "skip_host_skill_discovery", "--disable", "shell_tool", "--disable", "unified_exec", "--disable", "shell_snapshot", "--disable", "code_mode", "--disable", "code_mode_host", "--disable", "code_mode_only", "--disable", "multi_agent", "--disable", "browser_use", "--disable", "browser_use_external", "--disable", "browser_use_full_cdp_access", "--disable", "in_app_browser", "--disable", "computer_use", "--disable", "apps", "--disable", "plugins", "--disable", "plugin_sharing", "--disable", "remote_plugin", "--disable", "hooks", "--disable", "skill_search", "--disable", "skill_mcp_dependency_install", "--disable", "tool_suggest", "--disable", "tool_call_mcp_elicitation", "--disable", "auth_elicitation", "--disable", "goals", "--disable", "workspace_dependencies", "--disable", "in_app_chat", "--disable", "in_app_local_automation", "--disable", "in_app_updates", "--disable", "image_generation", "--disable", "view_image", "--skip-git-repo-check", "--sandbox", "read-only", "--color", "never", "--model", "gpt-test", "--cd", got.Dir, "--output-schema"}
	if len(got.Args) != len(wantPrefix)+4 || !reflect.DeepEqual(got.Args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("argv length = %d, prefix length = %d, argv = %#v, want = %#v", len(got.Args), len(wantPrefix), got.Args, wantPrefix)
	}
	if got.Args[len(got.Args)-3] != "--output-last-message" || got.Args[len(got.Args)-1] != "-" {
		t.Fatalf("argv tail = %#v", got.Args[len(got.Args)-3:])
	}
	wantStdin := "SYSTEM INSTRUCTIONS:\nsystem instruction\n\nUSER MESSAGE:\n" + payload.User()
	if got.Stdin != wantStdin {
		t.Fatalf("stdin = %q", got.Stdin)
	}
	if strings.Contains(got.Stdin, "sk-ant-api03-") {
		t.Fatal("unredacted input reached Codex")
	}
	if got.CodexHome == "" || pathWithin(got.Dir, got.CodexHome) || pathWithin(got.CodexHome, got.Dir) {
		t.Fatalf("CODEX_HOME %q is not isolated from working directory %q", got.CodexHome, got.Dir)
	}
	if runtime.GOOS != "windows" && got.CodexHomeMode != 0o700 {
		t.Fatalf("CODEX_HOME mode = %#o", got.CodexHomeMode)
	}
	if got.CodexAuthTarget != filepath.Join(os.Getenv("CODEX_HOME"), "auth.json") && !got.CodexAuthRegular {
		t.Fatalf("auth.json target = %q, regular = %v", got.CodexAuthTarget, got.CodexAuthRegular)
	}
	for _, arg := range got.Args {
		if got.CodexAuthTarget != "" && strings.Contains(arg, got.CodexAuthTarget) {
			t.Fatalf("authentication path leaked into argv: %q", arg)
		}
	}
	if _, err := os.Stat(got.CodexHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary CODEX_HOME was not removed: %v", err)
	}
	if len(got.EnvironmentLeaks) != 0 {
		t.Fatalf("environment reached Codex: %v", got.EnvironmentLeaks)
	}
}

// Temper's own configuration is the credential path of the HTTP provider. A CLI child runs on
// its own subscription authentication and has no use for it — and a subprocess that can read
// TEMPER_ENDPOINT or TEMPER_PROVIDER can also be steered by them. The assertion is exact
// absence of the whole prefix, not the absence of a list someone remembered to write down.
func TestCLIProvidersDoNotLeakTemperEnvironment(t *testing.T) {
	for name, makeProvider := range map[string]func(*testing.T) Provider{
		"codex":  func(t *testing.T) Provider { return newTestCodexProvider(t, "", helperTimeout) },
		"claude": func(*testing.T) Provider { return newClaudeProvider(helperCommand(), "", helperTimeout) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
			t.Setenv("TEMPER_API_KEY", "must-not-reach-the-cli")
			t.Setenv("TEMPER_PROVIDER", "http")
			t.Setenv("TEMPER_ENDPOINT", "https://gateway.attacker.example/v1")
			t.Setenv("TEMPER_MODEL", "must-not-reach-the-cli")
			t.Setenv("TEMPER_SOMETHING_ADDED_LATER", "must-not-reach-the-cli")
			t.Setenv("Temper_Api_Key", "must-not-reach-the-cli")
			t.Setenv("AUTOSKILLS_API_KEY", "legacy-must-not-reach-the-cli")
			t.Setenv("AUTOSKILLS_SOMETHING_ADDED_LATER", "legacy-must-not-reach-the-cli")

			out, err := makeProvider(t).Generate(context.Background(), preparedPayload(t))
			if err != nil {
				t.Fatal(err)
			}
			got := decodeInvocation(t, out)
			if len(got.ProductNames) != 0 {
				t.Fatalf("Temper or legacy AutoSkills configuration reached the CLI child: %v", got.ProductNames)
			}
			if len(got.EnvironmentLeaks) != 0 {
				t.Fatalf("provider credentials reached the CLI child: %v", got.EnvironmentLeaks)
			}
			// removing everything would satisfy the assertions above and break the provider
			if !got.PathPresent {
				t.Fatal("the child lost PATH: the filter removed the environment it needs to run")
			}
		})
	}
}

func TestCodexProviderRequiresAuthJSON(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	t.Setenv("CODEX_HOME", t.TempDir())
	_, err := newCodexProvider(helperCommand(), "", helperTimeout).Generate(context.Background(), preparedPayload(t))
	if !errors.Is(err, ErrNotAuthenticated) || !strings.Contains(err.Error(), "run codex login") {
		t.Fatalf("error = %v", err)
	}
}

func TestCodexProviderRejectsSymlinkedFinalOutput(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "symlink-output")
	_, err := newTestCodexProvider(t, "", helperTimeout).Generate(context.Background(), preparedPayload(t))
	if !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("error = %v", err)
	}
}

func TestCodexAuthenticationFallsBackToProtectedCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("production refuses credential copies on Windows")
	}
	source := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(source, []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "auth.json")
	err := stageCodexAuthWithLink(source, destination, func(string, string) error {
		return errors.New("links unavailable")
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "credential" {
		t.Fatalf("copied auth = %q", raw)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("copied auth mode = %#o", info.Mode().Perm())
	}
}

func TestCodexAuthenticationRefusesUnprotectedCopy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(source, []byte("credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "auth.json")
	err := stageCodexAuthWithLink(source, destination, func(string, string) error {
		return errors.New("links unavailable")
	}, false)
	if err == nil || !strings.Contains(err.Error(), "symbolic-link support") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("credential copy exists: %v", statErr)
	}
}

func TestClaudeProviderInvocationIsSafeAndNonPersistent(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-claude")
	t.Setenv("Anthropic_API_KEY", "must-not-reach-claude")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "must-not-reach-claude")
	t.Setenv("OPENAI_API_KEY", "must-not-reach-claude")
	t.Setenv("CODEX_ACCESS_TOKEN", "must-not-reach-claude")
	t.Setenv("Codex_Home", "must-not-reach-claude")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-reach-claude")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "must-not-reach-claude")
	p := newClaudeProvider(helperCommand(), "sonnet", helperTimeout)
	payload := preparedPayload(t)
	out, err := p.Generate(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeInvocation(t, out)
	wantPrefix := []string{"-p", "--safe-mode", "--disable-slash-commands", "--no-session-persistence", "--tools", "", "--permission-mode", "dontAsk", "--output-format", "json", "--json-schema", testOutputSchema, "--model", "sonnet", "--system-prompt-file"}
	if len(got.Args) != len(wantPrefix)+1 || !reflect.DeepEqual(got.Args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("argv = %#v\nwant prefix %#v", got.Args, wantPrefix)
	}
	if filepath.Dir(got.Args[len(got.Args)-1]) != got.Dir {
		t.Fatalf("system prompt file = %q", got.Args[len(got.Args)-1])
	}
	for _, arg := range got.Args {
		if strings.Contains(arg, payload.System()) {
			t.Fatalf("system prompt leaked into argv: %q", arg)
		}
	}
	if got.Stdin != payload.User() {
		t.Fatalf("stdin = %q", got.Stdin)
	}
	if got.SystemPrompt != payload.System() {
		t.Fatalf("system prompt = %q", got.SystemPrompt)
	}
	if strings.Contains(got.Stdin, "sk-ant-api03-") {
		t.Fatal("unredacted input reached Claude")
	}
	if !strings.Contains(filepath.Base(got.Dir), "temper-llm-") {
		t.Fatalf("working directory is not neutral: %q", got.Dir)
	}
	if len(got.EnvironmentLeaks) != 0 {
		t.Fatalf("environment reached Claude: %v", got.EnvironmentLeaks)
	}
	if got.CodexHome != "" {
		t.Fatalf("CODEX_HOME reached Claude: %q", got.CodexHome)
	}
}

func TestCLIProvidersRejectFailures(t *testing.T) {
	providers := map[string]func(time.Duration) Provider{
		"codex":  func(timeout time.Duration) Provider { return newTestCodexProvider(t, "", timeout) },
		"claude": func(timeout time.Duration) Provider { return newClaudeProvider(helperCommand(), "", timeout) },
	}
	for name, makeProvider := range providers {
		for _, tc := range []struct {
			behavior string
			want     error
		}{
			{behavior: "exit", want: ErrNotAuthenticated},
			{behavior: "not-logged-in", want: ErrNotAuthenticated},
			{behavior: "empty", want: ErrEmptyOutput},
			{behavior: "invalid", want: ErrInvalidOutput},
		} {
			t.Run(name+"/"+tc.behavior, func(t *testing.T) {
				t.Setenv("TEST_CLI_HELPER_BEHAVIOR", tc.behavior)
				if _, err := makeProvider(helperTimeout).Generate(context.Background(), preparedPayload(t)); !errors.Is(err, tc.want) {
					t.Fatalf("%s error = %v, want %v", tc.behavior, err, tc.want)
				}
			})
		}
		t.Run(name+"/caller_timeout", func(t *testing.T) {
			t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "sleep")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := makeProvider(helperTimeout).Generate(ctx, preparedPayload(t)); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
				t.Fatalf("timeout error = %v", err)
			}
		})
		t.Run(name+"/provider_timeout", func(t *testing.T) {
			t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "sleep")
			if _, err := makeProvider(20*time.Millisecond).Generate(context.Background(), preparedPayload(t)); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("provider timeout error = %v", err)
			}
		})
		t.Run(name+"/cancel", func(t *testing.T) {
			t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "sleep")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := makeProvider(helperTimeout).Generate(ctx, preparedPayload(t)); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation error = %v", err)
			}
		})
	}
}

func TestCLIProvidersBoundOutput(t *testing.T) {
	for name, provider := range map[string]Provider{
		"codex":  newTestCodexProvider(t, "", helperTimeout),
		"claude": newClaudeProvider(helperCommand(), "", helperTimeout),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "large")
			if _, err := provider.Generate(context.Background(), preparedPayload(t)); !errors.Is(err, ErrOutputTooLarge) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCLIProviderCancellationStopsDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-survived")
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "spawn-child")
	t.Setenv("TEST_CLI_HELPER_CHILD_MARKER", marker)

	// A fixed short deadline made this pass vacuously as soon as the helper got slower — race
	// instrumentation is enough — because a helper killed before it spawned anything also leaves no
	// surviving descendant. Cancellation is therefore driven by the descendant's own existence.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(helperTimeout)
		for time.Now().Before(deadline) && ctx.Err() == nil {
			if _, err := os.Stat(marker + ".spawned"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()

	_, err := newClaudeProvider(helperCommand(), "", helperTimeout).Generate(ctx, preparedPayload(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, statErr := os.Stat(marker + ".spawned"); statErr != nil {
		t.Fatalf("the descendant never started, so the kill proved nothing: %v", statErr)
	}
	// the descendant writes its own marker two seconds after starting; outliving the kill is the
	// exact failure this test exists for
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant survived cancellation: %v", err)
	}
}

func TestCLIProviderRefusesTemporaryDirectoryInsideExcludedRoot(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	excluded := t.TempDir()
	t.Setenv("TMPDIR", excluded)
	t.Setenv("TMP", excluded)
	t.Setenv("TEMP", excluded)
	var builder outbound.Builder
	builder.Static("user")
	payload, err := builder.BuildWithOutputSchema("system", testOutputSchema, excluded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newTestCodexProvider(t, "", helperTimeout).Generate(context.Background(), payload); err == nil || !strings.Contains(err.Error(), "neutral working directory") {
		t.Fatalf("error = %v", err)
	}
}

func TestCLIProviderAcceptsMissingExcludedRoot(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	excluded := filepath.Join(t.TempDir(), "deleted-repository")
	var builder outbound.Builder
	builder.Static("user")
	payload, err := builder.BuildWithOutputSchema("system", testOutputSchema, excluded)
	if err != nil {
		t.Fatal(err)
	}
	out, err := newClaudeProvider(helperCommand(), "", helperTimeout).Generate(context.Background(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeInvocation(t, out).Dir; pathWithin(excluded, got) {
		t.Fatalf("working directory %q is inside missing excluded root %q", got, excluded)
	}
}

func TestCLIProviderResolvesSymlinkedTemporaryRoot(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	excluded := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(excluded, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	t.Setenv("TMP", alias)
	t.Setenv("TEMP", alias)
	var builder outbound.Builder
	builder.Static("user")
	payload, err := builder.BuildWithOutputSchema("system", testOutputSchema, excluded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newClaudeProvider(helperCommand(), "", helperTimeout).Generate(context.Background(), payload); err == nil || !strings.Contains(err.Error(), "neutral working directory") {
		t.Fatalf("error = %v", err)
	}
}

func TestCLIProviderRefusesInstructionBearingTemporaryAncestor(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "AGENTS.md"), []byte("instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	temporaryBase := filepath.Join(base, "tmp")
	if err := os.Mkdir(temporaryBase, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temporaryBase)
	t.Setenv("TMP", temporaryBase)
	t.Setenv("TEMP", temporaryBase)
	if _, err := newTestCodexProvider(t, "", helperTimeout).Generate(context.Background(), preparedPayload(t)); err == nil || !strings.Contains(err.Error(), "configuration or instructions") {
		t.Fatalf("error = %v", err)
	}
}

func TestCLIProviderRefusesConfigurationBearingTemporaryAncestor(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	temporaryBase := filepath.Join(base, "tmp")
	if err := os.Mkdir(temporaryBase, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temporaryBase)
	t.Setenv("TMP", temporaryBase)
	t.Setenv("TEMP", temporaryBase)
	if _, err := newClaudeProvider(helperCommand(), "", helperTimeout).Generate(context.Background(), preparedPayload(t)); err == nil || !strings.Contains(err.Error(), "configuration or instructions") {
		t.Fatalf("error = %v", err)
	}
}

func TestCLIProviderAllowsUserConfigurationBoundary(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	home := filepath.Join(t.TempDir(), "home")
	temporaryBase := filepath.Join(home, "tmp")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte("global instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(temporaryBase, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TMPDIR", temporaryBase)
	t.Setenv("TMP", temporaryBase)
	t.Setenv("TEMP", temporaryBase)
	out, err := newClaudeProvider(helperCommand(), "", helperTimeout).Generate(context.Background(), preparedPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeInvocation(t, out).Dir; !pathWithin(home, got) {
		t.Fatalf("working directory %q is outside user home %q", got, home)
	}
}

func TestPathWithinUsesPathComponents(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", "repo")
	for _, tc := range []struct {
		target string
		want   bool
	}{
		{target: root, want: true},
		{target: filepath.Join(root, "tmp"), want: true},
		{target: filepath.Join(string(filepath.Separator), "tmp", "repo-other"), want: false},
		{target: filepath.Join(string(filepath.Separator), "tmp", "..cache"), want: false},
	} {
		if got := pathWithin(root, tc.target); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", root, tc.target, got, tc.want)
		}
	}
}

func TestCLIProviderRefusesTemporaryDirectoryInsideCallerTree(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	if _, err := newClaudeProvider(helperCommand(), "", helperTimeout).Generate(context.Background(), preparedPayload(t)); err == nil || !strings.Contains(err.Error(), "neutral working directory") {
		t.Fatalf("error = %v", err)
	}
}

func TestCLIProviderWorksFromFilesystemRoot(t *testing.T) {
	t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "success")
	t.Chdir(filepath.VolumeName(string(filepath.Separator)) + string(filepath.Separator))
	out, err := newClaudeProvider(helperCommand(), "", helperTimeout).Generate(context.Background(), preparedPayload(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeInvocation(t, out).Dir; !strings.Contains(filepath.Base(got), "temper-llm-") {
		t.Fatalf("working directory is not neutral: %q", got)
	}
}

func TestCLIErrorKeepsCauseWithoutEchoingPrompt(t *testing.T) {
	var builder outbound.Builder
	builder.Data("authentication required in private prompt", 0)
	payload, err := builder.BuildWithOutputSchema("system", testOutputSchema)
	if err != nil {
		t.Fatal(err)
	}
	for name, provider := range map[string]Provider{
		"codex":  newTestCodexProvider(t, "", helperTimeout),
		"claude": newClaudeProvider(helperCommand(), "", helperTimeout),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TEST_CLI_HELPER_BEHAVIOR", "request-error")
			_, err := provider.Generate(context.Background(), payload)
			if err == nil || !strings.Contains(err.Error(), "schema rejected") {
				t.Fatalf("error = %v", err)
			}
			if errors.Is(err, ErrNotAuthenticated) {
				t.Fatalf("prompt text caused false authentication classification: %v", err)
			}
			if strings.Contains(err.Error(), "SYSTEM INSTRUCTIONS") || strings.Contains(err.Error(), "private prompt") {
				t.Fatalf("error echoed prompt: %v", err)
			}
		})
	}
}

func TestCLIAuthenticationClassificationDoesNotEchoPrompt(t *testing.T) {
	for _, tc := range []struct {
		behavior string
		prompt   string
		wantAuth bool
	}{
		{behavior: "authentication-required", prompt: "authentication guidance", wantAuth: true},
		{behavior: "claude-auth-json", prompt: "private project context", wantAuth: true},
		{behavior: "claude-prompt-auth-json", prompt: "authentication required in private prompt", wantAuth: false},
	} {
		t.Run(tc.behavior, func(t *testing.T) {
			t.Setenv("TEST_CLI_HELPER_BEHAVIOR", tc.behavior)
			var builder outbound.Builder
			builder.Data(tc.prompt, 0)
			payload, err := builder.BuildWithOutputSchema("system", testOutputSchema)
			if err != nil {
				t.Fatal(err)
			}
			_, err = newClaudeProvider(helperCommand(), "", helperTimeout).Generate(context.Background(), payload)
			if err == nil {
				t.Fatal("expected provider error")
			}
			if errors.Is(err, ErrNotAuthenticated) != tc.wantAuth {
				t.Fatalf("error = %v, want authentication classification %v", err, tc.wantAuth)
			}
			if strings.Contains(err.Error(), tc.prompt) {
				t.Fatalf("error echoed prompt: %v", err)
			}
		})
	}
}

func TestCLIErrorDoesNotEchoPromptFragments(t *testing.T) {
	for _, tc := range []struct {
		behavior string
		system   string
		user     string
		leak     string
	}{
		{behavior: "prompt-error", system: "system", user: "private-customer-project-codename", leak: "private-customer-project-codename"},
		{behavior: "prompt-json-error", system: "system", user: "private-customer-project-codename", leak: "private-customer-project-codename"},
		{behavior: "short-prompt-error", system: "system", user: "x7", leak: "x7"},
		{behavior: "system-prompt-error", system: "system-private-customer-codename", user: "user", leak: "system-private-customer-codename"},
		{behavior: "prompt-partial-token-error", system: "privatecustomerprojectcodename", user: "user", leak: "customerproject"},
	} {
		t.Run(tc.behavior, func(t *testing.T) {
			t.Setenv("TEST_CLI_HELPER_BEHAVIOR", tc.behavior)
			var builder outbound.Builder
			builder.Data(tc.user, 0)
			payload, err := builder.BuildWithOutputSchema(tc.system, testOutputSchema)
			if err != nil {
				t.Fatal(err)
			}
			_, err = newClaudeProvider(helperCommand(), "", helperTimeout).Generate(context.Background(), payload)
			if err == nil || !strings.Contains(err.Error(), "exited non-zero") {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(err.Error(), tc.leak) {
				t.Fatalf("error echoed prompt: %v", err)
			}
		})
	}
}

func TestProviderGenerateAcceptsOnlyPreparedPayload(t *testing.T) {
	method, ok := reflect.TypeOf((*Provider)(nil)).Elem().MethodByName("Generate")
	if !ok {
		t.Fatal("Generate missing")
	}
	want := reflect.TypeOf(func(context.Context, outbound.Payload) (string, error) { return "", nil })
	if method.Type != want {
		t.Fatalf("Generate type = %v, want %v", method.Type, want)
	}
}
