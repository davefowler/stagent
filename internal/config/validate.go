package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Validate enforces the structural rules in notes/config.md
// "Schema rules" plus the v0.1-scope restrictions in
// decisions.md §6 (no `human` stages, no `tick` hooks, no
// run/forever bounds). Errors carry enough context to surface
// directly to the user.
//
// Validate does NOT check hook names against a registry — that's
// hooks-package territory and runs separately at runner start. It
// only checks structure, references, and v0.1 deferrals.
func (c *Config) Validate() error {
	if len(c.Stages) == 0 {
		return fmt.Errorf("no stages defined")
	}
	if len(c.Flows) == 0 {
		return fmt.Errorf("no flows defined")
	}

	if err := c.validateRoles(); err != nil {
		return err
	}
	if err := c.validateStages(); err != nil {
		return err
	}
	if err := c.validateFlows(); err != nil {
		return err
	}
	if err := c.validateCommands(); err != nil {
		return err
	}
	return nil
}

// identRE accepts the bare identifiers we use for stage / role /
// flow / command names. Conservative on purpose — these end up in
// file paths (`prompts/stages/<name>.md`) and event log payloads.
var identRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func validateIdent(kind, name string) error {
	if !identRE.MatchString(name) {
		return fmt.Errorf("%s name %q is not a valid identifier "+
			"(must match [a-z][a-z0-9_]*)", kind, name)
	}
	return nil
}

func (c *Config) validateRoles() error {
	for name, r := range c.Roles {
		if err := validateIdent("role", name); err != nil {
			return err
		}
		if r.Model == "" {
			return fmt.Errorf("role %q: model is required", name)
		}
		switch r.Bound {
		case BoundStage, BoundTask:
			// OK
		case BoundRun, BoundForever:
			return fmt.Errorf("role %q: bound %q is reserved for v0.2; use stage or task",
				name, r.Bound)
		default:
			return fmt.Errorf("role %q: unknown bound %q (want stage|task)", name, r.Bound)
		}
	}
	return nil
}

func (c *Config) validateStages() error {
	for name, s := range c.Stages {
		if err := validateIdent("stage", name); err != nil {
			return err
		}
		switch s.Type {
		case StageAgent:
			if s.Role == "" {
				return fmt.Errorf("stage %q: agent stages require role", name)
			}
			if _, ok := c.Roles[s.Role]; !ok {
				return fmt.Errorf("stage %q: role %q is not defined", name, s.Role)
			}
			if len(s.Hooks.Tick) > 0 {
				return fmt.Errorf("stage %q: agent stages cannot declare tick hooks "+
					"(agents own their own turn)", name)
			}
		case StageScript:
			if s.Role != "" {
				return fmt.Errorf("stage %q: script stages must not declare a role", name)
			}
		case StageHuman:
			return fmt.Errorf("stage %q: human stage type is deferred to v0.2", name)
		default:
			return fmt.Errorf("stage %q: unknown type %q (want agent|script)", name, s.Type)
		}

		if s.MaxRuns < 0 {
			return fmt.Errorf("stage %q: max_runs must be ≥ 0", name)
		}

		// v0.1 ships enter + exit only; tick is deferred to v0.2.
		if len(s.Hooks.Tick) > 0 {
			return fmt.Errorf("stage %q: tick hooks are deferred to v0.2", name)
		}

		if err := validateHookList(name, "enter", s.Hooks.Enter); err != nil {
			return err
		}
		if err := validateHookList(name, "exit", s.Hooks.Exit); err != nil {
			return err
		}
	}
	return nil
}

func validateHookList(stage, slot string, list []HookSpec) error {
	for i, h := range list {
		if strings.TrimSpace(h.Name) == "" {
			return fmt.Errorf("stage %q: %s hook #%d has empty name", stage, slot, i+1)
		}
		// We don't enforce a known-hooks list here — the hooks
		// registry catches unknown names with a clearer message
		// once it tries to instantiate.
	}
	return nil
}

func (c *Config) validateFlows() error {
	if _, ok := c.Flows["default"]; !ok {
		return fmt.Errorf("flow %q is required", "default")
	}
	for name, flow := range c.Flows {
		if err := validateIdent("flow", name); err != nil {
			return err
		}
		if len(flow) == 0 {
			return fmt.Errorf("flow %q is empty", name)
		}
		seen := map[string]int{}
		for i, stage := range flow {
			if _, ok := c.Stages[stage]; !ok {
				return fmt.Errorf("flow %q: stage %q (position %d) is not defined",
					name, stage, i+1)
			}
			if prev, dup := seen[stage]; dup {
				return fmt.Errorf("flow %q: stage %q appears at positions %d and %d "+
					"(stage names must be unique within a flow)",
					name, stage, prev+1, i+1)
			}
			seen[stage] = i
		}
	}
	return nil
}

func (c *Config) validateCommands() error {
	for name, cmd := range c.Commands {
		if err := validateIdent("command", name); err != nil {
			return err
		}
		if strings.TrimSpace(cmd.Run) == "" {
			return fmt.Errorf("command %q: run is required", name)
		}
	}
	return nil
}
