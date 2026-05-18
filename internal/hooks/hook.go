// Package hooks is the deterministic Go side of stagent's
// completion model. The agent exits; the runner runs the stage's
// exit hooks; the verdicts decide stage.completed vs retry vs
// redirect vs fail.
//
// See notes/architecture.md "How completion works" and
// notes/config.md "Hooks reference (v1)".
package hooks

import (
	"context"
	"time"

	"github.com/davefowler/stagent/internal/config"
	"github.com/davefowler/stagent/internal/events"
)

// Verdict is the result of one hook's evaluation.
type Verdict int

const (
	// Pass: this hook is satisfied. The stage completes if every
	// hook in the batch also passes.
	Pass Verdict = iota

	// NotYet: tick hooks only — keep waiting. v0.1 rejects tick
	// hooks at config-validation time, so the runner won't see
	// NotYet, but we define it so the interface stays stable for
	// v0.2.
	NotYet

	// Fail: this hook rejects the stage. Triggers retry if budget
	// allows, else stage.failed.
	Fail

	// Redirect: route to a named target stage, with Message
	// prepended to that stage's resume prompt.
	Redirect
)

func (v Verdict) String() string {
	switch v {
	case Pass:
		return "pass"
	case NotYet:
		return "not_yet"
	case Fail:
		return "fail"
	case Redirect:
		return "redirect"
	default:
		return "unknown"
	}
}

// Result is one hook's verdict plus diagnostic.
type Result struct {
	Verdict Verdict

	// Target is the stage name to redirect to. Only meaningful when
	// Verdict == Redirect.
	Target string

	// Message is human-readable. On Fail it is logged and (for
	// agent stages) prepended to the resume prompt. On Redirect it
	// becomes the redirect message.
	Message string
}

// Hook is the runtime interface. A Hook is instantiated once per
// config-load from a config.HookSpec via the registry, then Run
// repeatedly across stage transitions for the lifetime of the
// runner.
type Hook interface {
	// Name returns the hook's wire name (e.g. "section_check"). Used
	// in hook.fired event payloads and error messages.
	Name() string

	// Run evaluates the hook against ctx. Implementations must be
	// safe to call from multiple goroutines concurrently — different
	// task workers may invoke the same hook against different tasks
	// in parallel.
	Run(ctx context.Context, hctx *Ctx) Result
}

// Ctx carries the per-invocation inputs a hook needs.
type Ctx struct {
	// Task is the projected task state at the moment hooks run.
	// Hooks template against this (e.g. {{.Task.WorktreeDir}}).
	Task events.Task

	// Stage is the stage the hook is firing on.
	Stage string

	// Role is empty for non-agent stages; the role name otherwise.
	Role string

	// TaskFile is the contents of the task markdown at hook time.
	// The runner reads it once per hook batch so all hooks in the
	// batch see a consistent snapshot (an agent's edits during the
	// stage are visible; later hooks see the same bytes).
	TaskFile []byte

	// TaskFilePath is the absolute path to the task file. Used by
	// hooks that need to surface it in error messages.
	TaskFilePath string

	// Config is the loaded .stagent.yaml. The validator hook walks
	// it; other hooks rarely touch it.
	Config *config.Config

	// WorkingDir is the directory shell hooks default to. Typically
	// the runner's process CWD (the repo root containing .stagent/).
	WorkingDir string
}

// DefaultShellTimeout caps run_shell hooks when the config doesn't
// override it. Long enough for `git rebase` and dependency installs,
// short enough that a misconfigured hook can't wedge a worker.
const DefaultShellTimeout = 5 * time.Minute
