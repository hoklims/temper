// Package collector discovers and ingests agent transcripts from known local session stores,
// emitting canonical sessions (PRD §6.1). First-run discovery is a registry sweep — never a
// disk crawl.
package collector

import (
	"os"
	"path/filepath"

	"github.com/hoklims/temper/internal/canon"
)

// Adapter is the contract a tool integration implements. Supporting a new tool = one adapter.
type Adapter interface {
	// Tool returns the canonical tool id ("cursor", "claude", ...).
	Tool() string
	// SessionFiles lists absolute paths of transcript files, newest first.
	SessionFiles() ([]string, error)
	// Parse converts one transcript file into a canonical session.
	Parse(path string) (*canon.Session, error)
}

// DiscoveryEntry is one row of the first-run discovery report.
type DiscoveryEntry struct {
	Tool     string `json:"tool"`
	Root     string `json:"root"`
	Sessions int    `json:"sessions"`
}

// Discover sweeps all registered adapters and reports what exists on this machine.
func Discover(adapters []Adapter, roots map[string]string) []DiscoveryEntry {
	var out []DiscoveryEntry
	for _, a := range adapters {
		files, err := a.SessionFiles()
		if err != nil {
			continue
		}
		out = append(out, DiscoveryEntry{Tool: a.Tool(), Root: roots[a.Tool()], Sessions: len(files)})
	}
	return out
}

// HomePath joins segments under the user home dir, returning "" when home is unknown.
func HomePath(segments ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home}, segments...)...)
}
