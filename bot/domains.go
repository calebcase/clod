package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/calebcase/oops"
)

// DomainRegistry manages discovered domain directories. Each subdirectory of
// the workspace base path that has a `.clod/` is a domain — a specific area
// of work with its own onboarding README and isolated container setup.
type DomainRegistry struct {
	basePath string
	domains  map[string]string // name -> absolute path
}

// NewDomainRegistry creates a registry and discovers domains under basePath
// (the workspace).
func NewDomainRegistry(basePath string) (*DomainRegistry, error) {
	absPath, err := filepath.Abs(basePath)
	if err != nil {
		return nil, oops.Trace(err)
	}

	r := &DomainRegistry{
		basePath: absPath,
		domains:  make(map[string]string),
	}

	if err := r.Discover(); err != nil {
		return nil, err
	}

	return r, nil
}

// Discover scans the workspace for domain directories — subdirectories that
// have a `.clod/system/run` script.
func (r *DomainRegistry) Discover() error {
	entries, err := os.ReadDir(r.basePath)
	if err != nil {
		return oops.Trace(err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		domainPath := filepath.Join(r.basePath, name)
		clodPath := filepath.Join(domainPath, ".clod")

		info, err := os.Stat(clodPath)
		if err != nil || !info.IsDir() {
			continue
		}

		runPath := filepath.Join(clodPath, "system", "run")
		if _, err := os.Stat(runPath); err != nil {
			continue
		}

		r.domains[strings.ToLower(name)] = domainPath
	}

	return nil
}

// Get returns the absolute path for a domain by name. Revalidates the cached
// entry against the filesystem so a domain whose directory was removed since
// discovery gets evicted and treated as unknown — otherwise the caller would
// run clod against a non-existent path instead of triggering the init prompt
// flow.
func (r *DomainRegistry) Get(name string) (string, error) {
	key := strings.ToLower(name)
	path, ok := r.domains[key]
	if !ok {
		return "", oops.New("unknown domain: %q", name)
	}
	if _, err := os.Stat(filepath.Join(path, ".clod", "system", "run")); err != nil {
		delete(r.domains, key)
		return "", oops.New("unknown domain: %q", name)
	}
	return path, nil
}

// BasePath returns the absolute path of the workspace the registry searches.
func (r *DomainRegistry) BasePath() string {
	return r.basePath
}

// Refresh re-runs discovery so newly-initialized domains become visible
// without restarting the bot.
func (r *DomainRegistry) Refresh() error {
	r.domains = make(map[string]string)
	return r.Discover()
}

// List returns all registered domain names, alphabetically.
func (r *DomainRegistry) List() []string {
	names := make([]string, 0, len(r.domains))
	for name := range r.domains {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ListFormatted returns a human-readable summary of available domains.
func (r *DomainRegistry) ListFormatted() string {
	names := r.List()
	if len(names) == 0 {
		return "No domains available."
	}
	return "Available domains: " + strings.Join(names, ", ")
}
