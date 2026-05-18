package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/davefowler/stagent/internal/config"
	"github.com/davefowler/stagent/internal/events"
	"github.com/spf13/cobra"
)

// openStore opens the per-project event log at
// <working-dir>/.stagent/stagent.db, creating the directory if
// missing. Callers must Close the returned store.
func openStore(workingDir string) (*events.Store, error) {
	dir := filepath.Join(workingDir, ".stagent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return events.Open(context.Background(), filepath.Join(dir, "stagent.db"))
}

// loadConfig honors --config or falls back to
// <working-dir>/.stagent.yaml. Returns a friendly error if the
// file isn't present so users see "run stagent init" rather than
// a generic ENOENT.
func loadConfig(cmd *cobra.Command) (*config.Config, error) {
	path, err := configPath(cmd)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf("%s not found; run `stagent init` first", path)
	}
	return config.Load(path)
}
