// Package config loads, validates, and exposes the contents of
// .stagent.yaml. See notes/config.md for the schema reference and
// notes/decisions.md (§13, §14) for library / version policy.
//
// The package owns YAML decoding and structural validation. It does
// NOT know which hook names are valid — that lives in internal/hooks
// (the registry) so config stays decoupled from concrete hook code.
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root of .stagent.yaml.
type Config struct {
	Project   string             `yaml:"project,omitempty"`
	Roles     map[string]Role    `yaml:"roles"`
	TasksDir  string             `yaml:"tasks_dir,omitempty"`
	Stages    map[string]Stage   `yaml:"stages"`
	Flows     map[string]Flow    `yaml:"flows"`
	Commands  map[string]Command `yaml:"commands,omitempty"`
	Heartbeat Heartbeat          `yaml:"heartbeat,omitempty"`
}

// Role declares an agent identity. Role prompt is loaded by
// convention from .stagent/prompts/roles/<name>.md.
type Role struct {
	Model     string `yaml:"model"`
	Dangerous bool   `yaml:"dangerous,omitempty"`
	Bound     Bound  `yaml:"bound,omitempty"`
}

// Bound controls the Claude session scope per role. See
// notes/architecture.md "Sessions".
type Bound string

const (
	BoundStage   Bound = "stage"
	BoundTask    Bound = "task"
	BoundRun     Bound = "run"     // v0.1: rejected by validation
	BoundForever Bound = "forever" // v0.1: rejected by validation
)

// DefaultBound is applied when a role omits `bound:` — see
// notes/architecture.md "Sessions" / decisions.md §13.
const DefaultBound = BoundTask

// StageType is one of agent/human/script. Decisions.md §6 defers
// `human` to v0.2; validation rejects it in v0.1.
type StageType string

const (
	StageAgent  StageType = "agent"
	StageHuman  StageType = "human"  // v0.1: rejected by validation
	StageScript StageType = "script"
)

// Stage is one entry in the `stages:` map.
type Stage struct {
	Type    StageType  `yaml:"type"`
	Role    string     `yaml:"role,omitempty"`
	MaxRuns int        `yaml:"max_runs,omitempty"`
	Hooks   StageHooks `yaml:"hooks,omitempty"`
}

// StageHooks is the per-stage hook collection. v0.1 ships enter/exit
// only; tick is part of the schema for forward-compat but rejected
// at validation time.
type StageHooks struct {
	Enter []HookSpec `yaml:"enter,omitempty"`
	Exit  []HookSpec `yaml:"exit,omitempty"`
	Tick  []HookSpec `yaml:"tick,omitempty"`
}

// HookSpec is one hook entry in a YAML list. It's deliberately
// opaque (Name + raw Args) — the hooks package interprets Args
// per Name via its registry. That keeps config.go from needing to
// know about every hook's argument schema.
type HookSpec struct {
	Name string
	Args map[string]any
}

// UnmarshalYAML decodes a single-key mapping like
//
//	- run_shell:
//	    cmd: "..."
//	    fail_on_nonzero: true
//
// into HookSpec{Name: "run_shell", Args: {...}}. Empty argument
// objects (`name: {}`) and explicit nulls both decode to a non-nil
// empty Args map so callers don't need a nil check.
func (h *HookSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("hook entry must be a mapping (line %d, col %d)",
			value.Line, value.Column)
	}
	if len(value.Content) != 2 {
		return fmt.Errorf("hook entry must have exactly one key (line %d)", value.Line)
	}
	nameNode := value.Content[0]
	argsNode := value.Content[1]

	if nameNode.Kind != yaml.ScalarNode {
		return fmt.Errorf("hook name must be a string (line %d)", nameNode.Line)
	}
	h.Name = nameNode.Value
	h.Args = map[string]any{}

	switch argsNode.Kind {
	case yaml.MappingNode:
		if err := argsNode.Decode(&h.Args); err != nil {
			return fmt.Errorf("hook %q args: %w", h.Name, err)
		}
	case yaml.ScalarNode:
		// `name:` (null) or `name: ~` (also null) → empty args.
		if argsNode.Tag != "!!null" && argsNode.Value != "" {
			return fmt.Errorf("hook %q args must be a mapping (line %d)", h.Name, argsNode.Line)
		}
	default:
		return fmt.Errorf("hook %q args must be a mapping (line %d)", h.Name, argsNode.Line)
	}
	return nil
}

// Flow is an ordered list of stage names. A task selects one at
// creation time (`stagent new "<title>" --flow <name>`).
type Flow []string

// Command is one entry in the optional `commands:` map — a user
// recipe. v0.1 ships the parser; the CLI dispatcher comes later.
type Command struct {
	Desc string `yaml:"desc,omitempty"`
	Run  string `yaml:"run"`
}

// Heartbeat is the runner's tick configuration.
type Heartbeat struct {
	Interval Duration `yaml:"interval,omitempty"`
}

// DefaultHeartbeatInterval is used when heartbeat.interval is unset.
// Matches the scaffold value so behavior is identical with or
// without an explicit setting.
const DefaultHeartbeatInterval = 2 * time.Second

// Duration is a time.Duration alias with a YAML unmarshaller that
// accepts Go duration strings ("2s", "30s", "1m30s") and integers
// (interpreted as nanoseconds — matching encoding/json's behavior).
type Duration time.Duration

// AsDuration returns the typed time.Duration. Convenience for
// callers that don't want to repeat the conversion.
func (d Duration) AsDuration() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		// Try string first.
		parsed, err := time.ParseDuration(value.Value)
		if err != nil {
			return fmt.Errorf("invalid duration %q (line %d): %w", value.Value, value.Line, err)
		}
		*d = Duration(parsed)
		return nil
	default:
		return fmt.Errorf("duration must be a scalar (line %d)", value.Line)
	}
}

// MaxRunsForType returns the default max_runs for a stage type per
// notes/architecture.md "Run budgets".
func MaxRunsForType(t StageType) int {
	switch t {
	case StageHuman:
		return 1
	default:
		return 3
	}
}
