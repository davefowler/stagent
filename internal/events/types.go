// Package events is the durable state of stagent. Everything else —
// tasks, sessions, stage progress — is a SQL view over this log.
//
// The log is append-only. State corrections happen by appending a
// corrective event, never by editing history. Triggers in the schema
// enforce this at the database level.
package events

import (
	"encoding/json"
	"time"
)

// Event is one row in the event log.
type Event struct {
	ID        int64
	TaskID    int64
	Type      EventType
	Stage     string
	Role      string
	Actor     ActorKind
	Payload   json.RawMessage
	CreatedAt time.Time
}

// EventType is the discrete set of things that can happen. The wire
// values are the literal strings stored in the database; changing one
// would invalidate existing logs.
type EventType string

const (
	EventTaskCreated   EventType = "task.created"
	EventTaskAborted   EventType = "task.aborted"
	EventTaskCompleted EventType = "task.completed"

	EventStageEntered   EventType = "stage.entered"
	EventStageCompleted EventType = "stage.completed"
	EventStageFailed    EventType = "stage.failed"

	EventSessionStarted EventType = "session.started"
	EventSessionEnded   EventType = "session.ended"
	EventSessionResumed EventType = "session.resumed"

	EventHookFired     EventType = "hook.fired"
	EventHumanApproved EventType = "human.approved"
	EventForceTick     EventType = "force_tick"
)

// ActorKind identifies what kind of caller emitted an event. The
// runner uses this for log filtering and the viewer uses it to color
// rows. Not load-bearing for projections.
type ActorKind string

const (
	ActorAgent     ActorKind = "agent"
	ActorHuman     ActorKind = "human"
	ActorHeartbeat ActorKind = "heartbeat"
	ActorSystem    ActorKind = "system"
)

// StageType is recorded on stage.entered so projections never need to
// read .stagent.yaml to know whether a stage is human, agent, or
// script. Mirrors config.StageType but lives here to keep the events
// package self-contained.
type StageType string

const (
	StageAgent  StageType = "agent"
	StageHuman  StageType = "human"
	StageScript StageType = "script"
)

// EnterReason is why a stage was entered. Recorded on stage.entered.
type EnterReason string

const (
	ReasonFlow      EnterReason = "flow"
	ReasonRetry     EnterReason = "retry"
	ReasonRedirect  EnterReason = "redirect"
	ReasonHumanGoto EnterReason = "human_goto"
)

// TaskStatus is the projected status of a task. Computed by the
// `tasks` view, not stored.
type TaskStatus string

const (
	TaskStatusActive        TaskStatus = "active"
	TaskStatusWaitingHuman  TaskStatus = "waiting_human"
	TaskStatusCompleted     TaskStatus = "completed"
	TaskStatusFailed        TaskStatus = "failed"
	TaskStatusAborted       TaskStatus = "aborted"
)

// StageStatus is the projected status of a (task, stage) pair.
// Computed by the `stage_progress` view.
type StageStatus string

const (
	StageStatusNotStarted  StageStatus = "not_started"
	StageStatusInProgress  StageStatus = "in_progress"
	StageStatusCompleted   StageStatus = "completed"
	StageStatusFailed      StageStatus = "failed"
)

// Payload structs. These are not stored — they are marshaled to JSON
// and put in events.payload. They exist to give callers typed access
// without sprinkling map[string]any across the codebase.

type TaskCreatedPayload struct {
	Title       string `json:"title"`
	Flow        string `json:"flow"`
	TaskFile    string `json:"task_file"`
	WorktreeDir string `json:"worktree_dir"`
	Branch      string `json:"branch"`
}

type TaskAbortedPayload struct {
	Reason string `json:"reason"`
}

type StageEnteredPayload struct {
	Attempt   int         `json:"attempt"`
	Reason    EnterReason `json:"reason"`
	StageType StageType   `json:"stage_type"`
	FromStage string      `json:"from_stage,omitempty"`
	// BudgetOverride is set true when a human used `goto --force`
	// to bypass max_runs (decisions.md §9).
	BudgetOverride bool `json:"budget_override,omitempty"`
}

type StageFailedPayload struct {
	Reason    string `json:"reason"`
	LastError string `json:"last_error,omitempty"`
}

type SessionStartedPayload struct {
	ClaudeSessionID string `json:"claude_session_id"`
	PID             int    `json:"pid"`
}

type SessionEndedPayload struct {
	Reason   string `json:"reason"`
	ExitCode int    `json:"exit_code"`
}

type SessionResumedPayload struct {
	ClaudeSessionID string `json:"claude_session_id"`
}

type HookFiredPayload struct {
	Hook    string `json:"hook"`
	Result  string `json:"result"`
	Message string `json:"message,omitempty"`
}

type HumanApprovedPayload struct {
	ActorUser string `json:"actor_user"`
}
