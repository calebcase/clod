package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/calebcase/oops"
)

// TaskRegistry manages discovered task directories.
type TaskRegistry struct {
	basePath string
	tasks    map[string]string // name -> absolute path
}

// NewTaskRegistry creates a new TaskRegistry and discovers tasks in basePath.
func NewTaskRegistry(basePath string) (*TaskRegistry, error) {
	absPath, err := filepath.Abs(basePath)
	if err != nil {
		return nil, oops.Trace(err)
	}

	r := &TaskRegistry{
		basePath: absPath,
		tasks:    make(map[string]string),
	}

	if err := r.Discover(); err != nil {
		return nil, err
	}

	return r, nil
}

// Discover scans basePath for directories containing .clod/ subdirectories.
func (r *TaskRegistry) Discover() error {
	entries, err := os.ReadDir(r.basePath)
	if err != nil {
		return oops.Trace(err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		taskPath := filepath.Join(r.basePath, name)
		clodPath := filepath.Join(taskPath, ".clod")

		// Check if .clod directory exists
		info, err := os.Stat(clodPath)
		if err != nil || !info.IsDir() {
			continue
		}

		// Check if run script exists
		runPath := filepath.Join(clodPath, "system", "run")
		if _, err := os.Stat(runPath); err != nil {
			// run doesn't exist, skip this task
			continue
		}

		r.tasks[strings.ToLower(name)] = taskPath
	}

	return nil
}

// Get returns the absolute path for a task by name. Revalidates the cached
// entry against the filesystem so a task whose directory was removed since
// discovery gets evicted and treated as unknown — otherwise the caller would
// run clod against a non-existent path instead of triggering the init
// prompt flow.
func (r *TaskRegistry) Get(name string) (string, error) {
	key := strings.ToLower(name)
	path, ok := r.tasks[key]
	if !ok {
		return "", oops.New("unknown task: %q", name)
	}
	if _, err := os.Stat(filepath.Join(path, ".clod", "system", "run")); err != nil {
		delete(r.tasks, key)
		return "", oops.New("unknown task: %q", name)
	}
	return path, nil
}

// BasePath returns the absolute path the registry was configured to search.
func (r *TaskRegistry) BasePath() string {
	return r.basePath
}

// Refresh re-runs task discovery so newly-initialized tasks become visible
// without restarting the bot.
func (r *TaskRegistry) Refresh() error {
	r.tasks = make(map[string]string)
	return r.Discover()
}

// List returns all registered task names.
func (r *TaskRegistry) List() []string {
	names := make([]string, 0, len(r.tasks))
	for name := range r.tasks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ListFormatted returns a formatted string of available tasks.
func (r *TaskRegistry) ListFormatted() string {
	names := r.List()
	if len(names) == 0 {
		return "No tasks available."
	}
	return "Available tasks: " + strings.Join(names, ", ")
}
