// Package config loads ~/.temper/config.json with legacy AutoSkills compatibility.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/hoklims/temper/internal/migrate"
)

type Config struct {
	Provider string `json:"provider"`
	// LLM endpoint base, e.g. "https://api.openai.com/v1", "https://api.anthropic.com/v1",
	// "http://localhost:11434/v1" (Ollama).
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model"`

	// MaxSuggestionsPerScan caps a single scan's output (suggestion-fatigue guard, PRD §6.2).
	MaxSuggestionsPerScan int `json:"max_suggestions_per_scan"`
	// MinConfidence drops low-confidence suggestions before they are stored.
	MinConfidence float64 `json:"min_confidence"`
	// IgnoreProjects lists project names or repo roots that must never be scanned.
	IgnoreProjects []string `json:"ignore_projects"`
	// TriggerPhrase tunes automation down from "everything" to "on demand": when set, only
	// sessions where the USER typed this phrase (case-insensitive) are distilled —
	// e.g. "temper this". Empty means distill every eligible session.
	TriggerPhrase string `json:"trigger_phrase"`
	// AutoAcceptThreshold is DEPRECATED and never acted upon (HOK-539). It used to write
	// high-confidence suggestions to disk at scan time, which let model-authored content become a
	// file with no human in the loop. The field is still parsed so an existing config keeps
	// loading; a non-zero value only earns a warning on `scan`. Nothing writes without review.
	AutoAcceptThreshold float64 `json:"auto_accept_threshold"`
	// SectionBudgetBytes caps the managed AGENTS.md section (anti-"Markdown poisoning":
	// context is a finite resource). On overflow the lowest-confidence skills are demoted to
	// on-demand skill files. Default 12000 (~3k tokens). Codex hard-caps AGENTS.md at 32KiB.
	SectionBudgetBytes int `json:"section_budget_bytes"`
	// DaemonIntervalMinutes is the periodic sweep interval for `temper daemon` (file
	// watching triggers scans sooner; this is the safety net). Default 30.
	DaemonIntervalMinutes int `json:"daemon_interval_minutes"`
}

func Dir() string {
	return migrate.CurrentDir()
}

func Path() string { return filepath.Join(Dir(), "config.json") }

func defaults() Config {
	return Config{
		Provider:              "http",
		Endpoint:              "https://api.anthropic.com/v1",
		Model:                 "claude-sonnet-4-5",
		MaxSuggestionsPerScan: 10,
		MinConfidence:         0.5,
	}
}

func parseProvider(value string) (string, error) {
	provider := strings.ToLower(strings.TrimSpace(value))
	switch provider {
	case "http", "codex", "claude":
		return provider, nil
	default:
		return "", fmt.Errorf("invalid LLM provider %q: expected http, codex, or claude", value)
	}
}

func officialHTTPProvider(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" {
		return ""
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "api.anthropic.com":
		return "anthropic"
	case "api.openai.com":
		return "openai"
	default:
		return ""
	}
}

func providerField(raw []byte) (json.RawMessage, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, false, err
	}
	object, ok := token.(json.Delim)
	if !ok || object != '{' {
		return nil, false, errors.New("configuration must be a JSON object")
	}
	var provider json.RawMessage
	found := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false, errors.New("configuration field name must be a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false, err
		}
		if !strings.EqualFold(key, "provider") {
			continue
		}
		if key != "provider" {
			return nil, false, fmt.Errorf("invalid provider field %q: expected provider", key)
		}
		if found {
			return nil, false, errors.New("duplicate provider field")
		}
		provider = value
		found = true
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false, err
	}
	return provider, found, nil
}

// Load reads the config file if present, then applies env overrides:
// TEMPER_PROVIDER, TEMPER_ENDPOINT, TEMPER_API_KEY, TEMPER_MODEL. Legacy AUTOSKILLS_* names remain
// accepted when the corresponding TEMPER_* variable is absent.
// Falls back to ANTHROPIC_API_KEY / OPENAI_API_KEY when no key is configured
// (matching the endpoint's provider when recognizable).
func Load() (Config, error) {
	cfg := defaults()
	modelConfigured := false
	raw, err := os.ReadFile(Path())
	switch {
	case err == nil:
		if jerr := json.Unmarshal(raw, &cfg); jerr != nil {
			return cfg, fmt.Errorf("parse %s: %w", Path(), jerr)
		}
		rawProvider, providerPresent, jerr := providerField(raw)
		if jerr != nil {
			return cfg, fmt.Errorf("parse %s: %w", Path(), jerr)
		}
		var fields map[string]json.RawMessage
		if jerr := json.Unmarshal(raw, &fields); jerr == nil {
			_, modelConfigured = fields["model"]
			if providerPresent {
				var provider *string
				if jerr := json.Unmarshal(rawProvider, &provider); jerr != nil {
					return cfg, fmt.Errorf("parse provider in %s: %w", Path(), jerr)
				}
				if provider == nil {
					return cfg, fmt.Errorf("invalid LLM provider null: expected http, codex, or claude")
				}
				cfg.Provider, jerr = parseProvider(*provider)
				if jerr != nil {
					return cfg, jerr
				}
			}
		}
	case errors.Is(err, os.ErrNotExist):
		// fine — defaults + env
	default:
		return cfg, err
	}

	fileProvider := cfg.Provider
	if value, present := environment("TEMPER_PROVIDER", "AUTOSKILLS_PROVIDER"); present {
		cfg.Provider, err = parseProvider(value)
		if err != nil {
			return cfg, err
		}
		if cfg.Provider != fileProvider && environmentValue("TEMPER_MODEL", "AUTOSKILLS_MODEL") == "" {
			cfg.Model = ""
			modelConfigured = false
		}
	}
	if cfg.Provider != "http" && !modelConfigured && environmentValue("TEMPER_MODEL", "AUTOSKILLS_MODEL") == "" {
		cfg.Model = ""
	}
	if v := environmentValue("TEMPER_ENDPOINT", "AUTOSKILLS_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}
	if v := environmentValue("TEMPER_MODEL", "AUTOSKILLS_MODEL"); v != "" {
		cfg.Model = v
	}
	if v := environmentValue("TEMPER_API_KEY", "AUTOSKILLS_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if cfg.Provider == "http" && cfg.APIKey == "" {
		// Only official HTTPS endpoints may inherit provider credentials. Custom gateways
		// must use api_key or TEMPER_API_KEY explicitly.
		switch officialHTTPProvider(cfg.Endpoint) {
		case "anthropic":
			if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" {
				cfg.APIKey = v
			}
		case "openai":
			if v := os.Getenv("OPENAI_API_KEY"); v != "" {
				cfg.APIKey = v
			}
		}
	}
	if cfg.MaxSuggestionsPerScan <= 0 {
		cfg.MaxSuggestionsPerScan = 10
	}
	if cfg.SectionBudgetBytes <= 0 {
		cfg.SectionBudgetBytes = 12000
	}
	if cfg.DaemonIntervalMinutes <= 0 {
		cfg.DaemonIntervalMinutes = 30
	}
	return cfg, nil
}

func environment(current, legacy string) (string, bool) {
	if value, present := os.LookupEnv(current); present {
		return value, true
	}
	return os.LookupEnv(legacy)
}

func environmentValue(current, legacy string) string {
	value, _ := environment(current, legacy)
	return value
}
