package config

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// DefaultTasksDir is the directory tasks are stored in if `tasks_dir`
// is unset. See notes/config.md and the scaffold default.
const DefaultTasksDir = "tasks"

// Load reads the YAML at path, applies defaults, and validates the
// result for v0.1 scope (decisions.md §6). It returns the validated
// Config or an error pointing at the offending file path so the CLI
// can surface it verbatim.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	cfg, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", path, err)
	}
	return cfg, nil
}

// Parse is Load split: decode bytes + apply defaults, no validation.
// Exported because tests and the runner's SIGHUP reload path benefit
// from separating "could YAML parse?" from "does the config make
// sense for the current binary?".
func Parse(src []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true) // surface unknown top-level keys as errors
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.TasksDir == "" {
		c.TasksDir = DefaultTasksDir
	}
	if c.Heartbeat.Interval == 0 {
		c.Heartbeat.Interval = Duration(DefaultHeartbeatInterval)
	}
	for name, r := range c.Roles {
		if r.Bound == "" {
			r.Bound = DefaultBound
			c.Roles[name] = r
		}
	}
	for name, s := range c.Stages {
		if s.MaxRuns == 0 {
			s.MaxRuns = MaxRunsForType(s.Type)
			c.Stages[name] = s
		}
	}
}

