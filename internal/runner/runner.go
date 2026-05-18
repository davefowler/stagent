// Package runner is the top-level state machine that drives tasks
// through their flows. One process per repo. The runner reads the
// event log + filesystem to derive state, never holds state in
// memory that would be lost on crash. See notes/architecture.md
// "Process model" and decisions.md §6.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/davefowler/stagent/internal/config"
	"github.com/davefowler/stagent/internal/events"
	"github.com/davefowler/stagent/internal/hooks"
)

// PIDFileName is the path under the working dir where the runner
// drops its PID. Per architecture.md "Liveness".
const PIDFileName = ".stagent/runner.pid"

// Options holds the runner's constructor inputs. All fields are
// required unless noted.
type Options struct {
	// WorkingDir is the project root. The runner writes its PID
	// file under here and uses it as cwd for shell hooks.
	WorkingDir string

	// Store is the event log. The runner does not Close it on stop
	// — that's the caller's responsibility.
	Store *events.Store

	// ConfigPath is the .stagent.yaml location for SIGHUP reloads.
	ConfigPath string

	// Config is the initially-loaded config.
	Config *config.Config

	// Registry holds the hook constructors. NewDefault() in
	// production, fakes in tests.
	Registry *hooks.Registry

	// ClaudeBin is the path to the claude binary (real or fraude).
	// Honors $STAGENT_CLAUDE_BIN when blank.
	ClaudeBin string

	// Logger receives structured runtime events. Defaults to a
	// JSON handler over stderr if nil.
	Logger *slog.Logger
}

// Runner drives the state machine. Construct with New, drive with
// Run(ctx), Stop() to terminate.
type Runner struct {
	opts Options

	// runtime is swapped atomically on SIGHUP so workers see a
	// consistent (Config, compiled hooks) pair without locks.
	runtime atomic.Pointer[runtime]

	workers   sync.Map // taskID(int64) → *worker
	workerWG  sync.WaitGroup
	stopCh    chan struct{}
	stopOnce  sync.Once

	signals chan os.Signal
}

// runtime is the swappable bundle of "what's loaded right now."
type runtime struct {
	cfg            *config.Config
	stageHooks     map[stageHookKey][]hooks.Hook // (stage, slot) → compiled list
	rolePrompt     map[string]string             // role → text loaded from disk
	stagePrompt    map[string]string             // stage → templated source
}

type stageHookKey struct {
	stage string
	slot  hookSlot
}

type hookSlot int

const (
	slotEnter hookSlot = iota
	slotExit
)

// New constructs a runner from Options. Compiles the initial
// hooks/prompts so config errors surface before Run.
func New(opts Options) (*Runner, error) {
	if opts.WorkingDir == "" {
		return nil, errors.New("runner: WorkingDir is required")
	}
	if opts.Store == nil {
		return nil, errors.New("runner: Store is required")
	}
	if opts.Config == nil {
		return nil, errors.New("runner: Config is required")
	}
	if opts.Registry == nil {
		opts.Registry = hooks.NewDefault()
	}
	if opts.ClaudeBin == "" {
		opts.ClaudeBin = os.Getenv("STAGENT_CLAUDE_BIN")
	}
	if opts.ClaudeBin == "" {
		opts.ClaudeBin = "claude"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}

	r := &Runner{
		opts:    opts,
		stopCh:  make(chan struct{}),
		signals: make(chan os.Signal, 4),
	}

	rt, err := buildRuntime(opts.WorkingDir, opts.Config, opts.Registry)
	if err != nil {
		return nil, err
	}
	r.runtime.Store(rt)
	return r, nil
}

// Run blocks until ctx is cancelled or Stop is called. Acquires the
// PID file, runs crash recovery, dispatches workers each heartbeat
// tick, and unwinds gracefully on shutdown.
func (r *Runner) Run(ctx context.Context) error {
	pidPath := filepath.Join(r.opts.WorkingDir, PIDFileName)
	if err := acquirePIDFile(pidPath); err != nil {
		return err
	}
	defer os.Remove(pidPath)

	r.opts.Logger.Info("runner started",
		"pid", os.Getpid(),
		"working_dir", r.opts.WorkingDir,
		"claude_bin", r.opts.ClaudeBin)

	// Signal handling: SIGHUP reload, SIGINT/SIGTERM graceful stop.
	signalNotify(r.signals)
	defer signalStop(r.signals)

	if err := r.recover(ctx); err != nil {
		r.opts.Logger.Error("recovery failed", "err", err)
		return fmt.Errorf("recovery: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go r.signalLoop(ctx, cancel)

	r.heartbeat(ctx)

	// Heartbeat exited (ctx done). Wait for in-flight workers.
	r.workerWG.Wait()
	r.opts.Logger.Info("runner stopped")
	return nil
}

// Stop signals the heartbeat loop to terminate. Safe to call
// multiple times.
func (r *Runner) Stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
}

// reload re-reads the config from disk and swaps the runtime
// atomically. Called from the signal loop on SIGHUP.
func (r *Runner) reload() {
	cfg, err := config.Load(r.opts.ConfigPath)
	if err != nil {
		r.opts.Logger.Error("SIGHUP reload failed; keeping previous config",
			"err", err, "path", r.opts.ConfigPath)
		return
	}
	rt, err := buildRuntime(r.opts.WorkingDir, cfg, r.opts.Registry)
	if err != nil {
		r.opts.Logger.Error("SIGHUP reload failed; keeping previous runtime",
			"err", err)
		return
	}
	r.runtime.Store(rt)
	r.opts.Logger.Info("config reloaded", "path", r.opts.ConfigPath)
}

// signalLoop translates OS signals into runner actions. Cancel
// terminates Run; SIGHUP triggers reload.
func (r *Runner) signalLoop(ctx context.Context, cancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-r.signals:
			switch sig {
			case syscall.SIGINT, syscall.SIGTERM:
				r.opts.Logger.Info("shutdown signal", "signal", sig.String())
				r.Stop()
				cancel()
				return
			case syscall.SIGHUP:
				r.reload()
			}
		}
	}
}

// buildRuntime compiles the per-stage hook lists and loads the
// per-role + per-stage prompts. Any error here means the config
// is broken; the caller surfaces it.
func buildRuntime(workingDir string, cfg *config.Config, reg *hooks.Registry) (*runtime, error) {
	rt := &runtime{
		cfg:         cfg,
		stageHooks:  map[stageHookKey][]hooks.Hook{},
		rolePrompt:  map[string]string{},
		stagePrompt: map[string]string{},
	}
	for name, stage := range cfg.Stages {
		enterHooks, err := reg.BuildList(stage.Hooks.Enter)
		if err != nil {
			return nil, fmt.Errorf("stage %q enter hooks: %w", name, err)
		}
		exitHooks, err := reg.BuildList(stage.Hooks.Exit)
		if err != nil {
			return nil, fmt.Errorf("stage %q exit hooks: %w", name, err)
		}
		rt.stageHooks[stageHookKey{name, slotEnter}] = enterHooks
		rt.stageHooks[stageHookKey{name, slotExit}] = exitHooks

		// Stage prompt (optional — script stages typically don't have one).
		stagePromptPath := filepath.Join(workingDir, ".stagent", "prompts", "stages", name+".md")
		if b, err := os.ReadFile(stagePromptPath); err == nil {
			rt.stagePrompt[name] = string(b)
		}
	}
	for role := range cfg.Roles {
		rolePromptPath := filepath.Join(workingDir, ".stagent", "prompts", "roles", role+".md")
		if b, err := os.ReadFile(rolePromptPath); err == nil {
			rt.rolePrompt[role] = string(b)
		} else if errors.Is(err, os.ErrNotExist) {
			// Role prompt missing is non-fatal — agents will run with
			// an empty system prompt. Logged at first use.
		} else {
			return nil, fmt.Errorf("role %q prompt: %w", role, err)
		}
	}
	return rt, nil
}

// current returns the latest runtime bundle. Workers must call this
// at safe stop-points (before each step) rather than caching, so
// SIGHUP reloads take effect promptly.
func (r *Runner) current() *runtime { return r.runtime.Load() }

// acquirePIDFile writes our PID to path. Refuses if another runner
// is alive (its PID responds to kill(0)). A stale PID file (process
// gone) is overwritten.
func acquirePIDFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if b, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(string(bytesTrim(b))); err == nil && pid > 0 {
			if isAlive(pid) {
				return fmt.Errorf("another runner is alive (pid %d, %s); stop it first", pid, path)
			}
		}
		// Stale file — fall through and overwrite.
	}
	pidStr := strconv.Itoa(os.Getpid())
	return os.WriteFile(path, []byte(pidStr+"\n"), 0o644)
}

func isAlive(pid int) bool {
	// Sending signal 0 doesn't deliver but does check liveness.
	err := syscall.Kill(pid, 0)
	return err == nil
}

func bytesTrim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ') {
		b = b[1:]
	}
	return b
}

// HeartbeatInterval returns the active tick rate. Exposed for tests
// that need to wait deterministically.
func (r *Runner) HeartbeatInterval() time.Duration {
	return r.current().cfg.Heartbeat.Interval.AsDuration()
}
