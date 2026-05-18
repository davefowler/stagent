package hooks

import (
	"fmt"
	"sort"

	"github.com/davefowler/stagent/internal/config"
)

// Constructor builds one concrete hook from its raw YAML args. The
// args map is what config.HookSpec.Args contains for that hook entry.
// Constructors validate args strictly: a typo'd key is a config
// error, surfaced at runner start (or on SIGHUP reload).
type Constructor func(args map[string]any) (Hook, error)

// Registry maps wire names ("run_shell", "section_check", ...) to
// constructors. NewDefault returns the v0.1 registry.
type Registry struct {
	constructors map[string]Constructor
}

// New returns an empty registry. Tests use this to install fakes.
func New() *Registry {
	return &Registry{constructors: map[string]Constructor{}}
}

// NewDefault returns a registry with all v0.1 hooks installed.
func NewDefault() *Registry {
	r := New()
	r.Register("run_shell", newRunShell)
	r.Register("section_check", newSectionCheck)
	r.Register("min_words", newMinWords)
	r.Register("validate_task_sections", newValidateTaskSections)
	return r
}

// Register adds a constructor. Re-registering overwrites; tests
// can use this to swap implementations.
func (r *Registry) Register(name string, c Constructor) {
	r.constructors[name] = c
}

// Names returns the registered hook names, sorted. Diagnostic.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.constructors))
	for k := range r.constructors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Build constructs one hook from a HookSpec.
func (r *Registry) Build(spec config.HookSpec) (Hook, error) {
	c, ok := r.constructors[spec.Name]
	if !ok {
		return nil, fmt.Errorf("unknown hook %q (registered: %v)", spec.Name, r.Names())
	}
	h, err := c(spec.Args)
	if err != nil {
		return nil, fmt.Errorf("hook %q: %w", spec.Name, err)
	}
	return h, nil
}

// BuildList constructs every hook in a list, surfacing the first
// failure with its index so users can find the offending entry.
func (r *Registry) BuildList(list []config.HookSpec) ([]Hook, error) {
	out := make([]Hook, 0, len(list))
	for i, spec := range list {
		h, err := r.Build(spec)
		if err != nil {
			return nil, fmt.Errorf("hook #%d: %w", i+1, err)
		}
		out = append(out, h)
	}
	return out, nil
}

// argString pulls a required string field from args. Returns a
// clear error if missing or wrong type.
func argString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("missing required arg %q", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("arg %q must be a string (got %T)", key, v)
	}
	return s, nil
}

// argOptionalString pulls an optional string field. Returns ("", nil)
// when absent.
func argOptionalString(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("arg %q must be a string (got %T)", key, v)
	}
	return s, nil
}

// argBool pulls an optional bool. Returns (default, nil) when absent.
func argBool(args map[string]any, key string, def bool) (bool, error) {
	v, ok := args[key]
	if !ok {
		return def, nil
	}
	b, ok := v.(bool)
	if !ok {
		return def, fmt.Errorf("arg %q must be a bool (got %T)", key, v)
	}
	return b, nil
}

// argInt pulls an optional int. Returns (default, nil) when absent.
// yaml.v3 decodes integers as int by default, but accept int64/float64
// too — both can appear from generic map[string]any decoding.
func argInt(args map[string]any, key string, def int) (int, error) {
	v, ok := args[key]
	if !ok {
		return def, nil
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	default:
		return def, fmt.Errorf("arg %q must be an integer (got %T)", key, v)
	}
}

// argOptionalMap pulls an optional sub-map. Returns (nil, nil) when
// absent. Used by section_check's on_fail block.
func argOptionalMap(args map[string]any, key string) (map[string]any, error) {
	v, ok := args[key]
	if !ok {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("arg %q must be a mapping (got %T)", key, v)
	}
	return m, nil
}

// ensureKnownArgs returns an error if args contains any keys not in
// allowed. Catches user typos at registry-build time rather than at
// stage-execution time.
func ensureKnownArgs(name string, args map[string]any, allowed ...string) error {
	allow := map[string]bool{}
	for _, k := range allowed {
		allow[k] = true
	}
	for k := range args {
		if !allow[k] {
			return fmt.Errorf("hook %q: unknown arg %q (allowed: %v)", name, k, allowed)
		}
	}
	return nil
}
