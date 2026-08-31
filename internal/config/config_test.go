package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, raw string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir(), "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSupportsLegacyEnvironmentWithTemperPrecedence(t *testing.T) {
	writeConfig(t, `{}`)
	for _, name := range []string{"TEMPER_PROVIDER", "TEMPER_MODEL", "AUTOSKILLS_PROVIDER", "AUTOSKILLS_MODEL"} {
		unsetEnv(t, name)
	}
	t.Setenv("AUTOSKILLS_PROVIDER", "codex")
	t.Setenv("AUTOSKILLS_MODEL", "legacy-model")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "codex" || cfg.Model != "legacy-model" {
		t.Fatalf("legacy env not applied: %+v", cfg)
	}

	t.Setenv("TEMPER_PROVIDER", "claude")
	t.Setenv("TEMPER_MODEL", "temper-model")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "claude" || cfg.Model != "temper-model" {
		t.Fatalf("Temper env did not win: %+v", cfg)
	}
}

func unsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProviderCompatibility(t *testing.T) {
	unsetEnv(t, "TEMPER_PROVIDER")
	t.Setenv("TEMPER_MODEL", "")
	t.Setenv("TEMPER_ENDPOINT", "")
	t.Setenv("TEMPER_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	for _, tc := range []struct {
		name  string
		raw   string
		want  string
		model string
	}{
		{name: "legacy", raw: `{"endpoint":"http://localhost:11434/v1","model":"local"}`, want: "http", model: "local"},
		{name: "explicit http", raw: `{"provider":"http","endpoint":"https://example.com/v1","api_key":"key","model":"m"}`, want: "http", model: "m"},
		{name: "codex", raw: `{"provider":"codex"}`, want: "codex", model: ""},
		{name: "claude", raw: `{"provider":"claude"}`, want: "claude", model: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(t, tc.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Provider != tc.want {
				t.Fatalf("provider = %q, want %q", cfg.Provider, tc.want)
			}
			if cfg.Model != tc.model {
				t.Fatalf("model = %q, want %q", cfg.Model, tc.model)
			}
		})
	}
}

func TestLoadRejectsUnknownProvider(t *testing.T) {
	unsetEnv(t, "TEMPER_PROVIDER")
	writeConfig(t, `{"provider":"other"}`)
	if _, err := Load(); err == nil {
		t.Fatal("unknown provider must fail")
	}
}

func TestLoadRejectsExplicitlyInvalidProvider(t *testing.T) {
	unsetEnv(t, "TEMPER_PROVIDER")
	for _, raw := range []string{
		`{"provider":null}`,
		`{"provider":""}`,
		`{"provider":" "}`,
		`{"provider":42}`,
		`{"Provider":"other"}`,
		`{"PROVIDER":null}`,
		`{"Provider":""}`,
		`{"Provider":"http","PROVIDER":"other"}`,
		`{"provider":"other","provider":"http"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			writeConfig(t, raw)
			if _, err := Load(); err == nil {
				t.Fatalf("provider in %s must fail", raw)
			}
		})
	}
}

func TestLoadRejectsInvalidFileProviderBeforeEnvironmentOverride(t *testing.T) {
	t.Setenv("TEMPER_PROVIDER", "codex")
	for _, raw := range []string{`{"provider":null}`, `{"Provider":"other"}`} {
		t.Run(raw, func(t *testing.T) {
			writeConfig(t, raw)
			if _, err := Load(); err == nil {
				t.Fatal("invalid file provider must fail before environment override")
			}
		})
	}
}

func TestLoadAcceptsValidProviderEnvironmentOverride(t *testing.T) {
	t.Setenv("TEMPER_PROVIDER", "claude")
	writeConfig(t, `{"provider":"http"}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "claude" {
		t.Fatalf("provider = %q", cfg.Provider)
	}
}

func TestLoadRejectsInvalidProviderEnvironment(t *testing.T) {
	writeConfig(t, `{}`)
	for _, value := range []string{"", " ", "other"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TEMPER_PROVIDER", value)
			if _, err := Load(); err == nil {
				t.Fatalf("provider environment %q must fail", value)
			}
		})
	}
}

func TestLoadOnlyInheritsCredentialsForOfficialHTTPSHosts(t *testing.T) {
	unsetEnv(t, "TEMPER_PROVIDER")
	unsetEnv(t, "TEMPER_API_KEY")
	t.Setenv("ANTHROPIC_API_KEY", "anthropic-secret")
	t.Setenv("OPENAI_API_KEY", "openai-secret")

	for _, tc := range []struct {
		name     string
		endpoint string
		want     string
	}{
		{name: "anthropic official", endpoint: "https://api.anthropic.com/v1", want: "anthropic-secret"},
		{name: "openai official", endpoint: "https://api.openai.com/v1", want: "openai-secret"},
		{name: "custom gateway", endpoint: "https://gateway.company.test/v1"},
		{name: "anthropic lookalike", endpoint: "https://api.anthropic.com.evil/v1"},
		{name: "openai lookalike", endpoint: "https://api.openai.com.evil/v1"},
		{name: "insecure anthropic", endpoint: "http://api.anthropic.com/v1"},
		{name: "insecure openai", endpoint: "http://api.openai.com/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(t, `{"provider":"http","endpoint":"`+tc.endpoint+`"}`)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.APIKey != tc.want {
				t.Fatalf("api key = %q, want %q", cfg.APIKey, tc.want)
			}
		})
	}
}

func TestLoadClearsInheritedModelWhenProviderChanges(t *testing.T) {
	unsetEnv(t, "TEMPER_MODEL")
	for _, tc := range []struct {
		name     string
		file     string
		provider string
	}{
		{name: "http to codex", file: `{"provider":"http","model":"http-model"}`, provider: "codex"},
		{name: "codex to claude", file: `{"provider":"codex","model":"codex-model"}`, provider: "claude"},
		{name: "claude to http", file: `{"provider":"claude","model":"claude-model"}`, provider: "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeConfig(t, tc.file)
			t.Setenv("TEMPER_PROVIDER", tc.provider)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Model != "" {
				t.Fatalf("model = %q, want empty after provider change", cfg.Model)
			}
		})
	}
}

func TestLoadKeepsExplicitEnvironmentModelWhenProviderChanges(t *testing.T) {
	writeConfig(t, `{"provider":"http","model":"http-model"}`)
	t.Setenv("TEMPER_PROVIDER", "codex")
	t.Setenv("TEMPER_MODEL", "codex-model")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "codex-model" {
		t.Fatalf("model = %q, want codex-model", cfg.Model)
	}
}
