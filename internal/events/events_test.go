package events

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore opens a fresh on-disk DB inside t.TempDir().
// In-memory shared-cache databases would be slightly faster, but the
// runner uses an on-disk file in real life and we want the test
// surface to match (WAL mode is a no-op on `:memory:`).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(context.Background(), filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func mustAppend(t *testing.T, s *Store, e *Event) *Event {
	t.Helper()
	if err := s.Append(context.Background(), e); err != nil {
		t.Fatalf("Append %s: %v", e.Type, err)
	}
	return e
}

func TestOpenAppliesSchema(t *testing.T) {
	store := newTestStore(t)

	// The events table exists and is empty.
	var n int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM events").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected empty events, got %d rows", n)
	}

	// All four views exist and are queryable on an empty log.
	for _, view := range []string{"tasks", "stage_progress", "sessions", "active_sessions"} {
		if _, err := store.DB().Query("SELECT * FROM " + view); err != nil {
			t.Fatalf("query view %s: %v", view, err)
		}
	}

	// WAL is on. Persisted across the open, so any connection sees it.
	var mode string
	if err := store.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("expected journal_mode=wal, got %q", mode)
	}
}

func TestAppendFillsIDAndCreatedAt(t *testing.T) {
	store := newTestStore(t)
	before := time.Now().Add(-time.Second)

	e := &Event{TaskID: 1, Type: EventTaskCreated, Actor: ActorSystem}
	if err := store.AppendPayload(context.Background(), e, TaskCreatedPayload{
		Title: "demo", Flow: "default", TaskFile: "tasks/001-demo.md",
		WorktreeDir: ".worktrees/task-001", Branch: "task-001",
	}); err != nil {
		t.Fatalf("AppendPayload: %v", err)
	}
	if e.ID == 0 {
		t.Fatal("expected ID to be set")
	}
	if e.CreatedAt.Before(before) {
		t.Fatalf("CreatedAt %v before %v", e.CreatedAt, before)
	}
}

func TestAppendValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		e    *Event
		want string
	}{
		{"nil event", nil, "nil event"},
		{"missing type", &Event{Actor: ActorSystem}, "missing Type"},
		{"missing actor", &Event{Type: EventTaskCreated}, "missing Actor"},
		{"invalid json", &Event{Type: EventTaskCreated, Actor: ActorSystem, Payload: json.RawMessage(`{not-json`)}, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := store.Append(ctx, tc.e)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestAppendOnlyTriggers(t *testing.T) {
	store := newTestStore(t)
	mustAppend(t, store, &Event{TaskID: 1, Type: EventTaskCreated, Actor: ActorSystem,
		Payload: json.RawMessage(`{}`)})

	if _, err := store.DB().Exec(`UPDATE events SET type = 'task.aborted' WHERE id = 1`); err == nil {
		t.Fatal("expected UPDATE to be blocked by trigger")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only error, got %v", err)
	}

	if _, err := store.DB().Exec(`DELETE FROM events WHERE id = 1`); err == nil {
		t.Fatal("expected DELETE to be blocked by trigger")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only error, got %v", err)
	}
}

func TestReadAfterAndMaxID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if max, err := store.MaxID(ctx); err != nil || max != 0 {
		t.Fatalf("MaxID on empty: got (%d, %v); want (0, nil)", max, err)
	}

	for i := 1; i <= 3; i++ {
		mustAppend(t, store, &Event{
			TaskID: int64(i), Type: EventTaskCreated, Actor: ActorSystem,
			Payload: json.RawMessage(`{}`),
		})
	}

	max, err := store.MaxID(ctx)
	if err != nil || max != 3 {
		t.Fatalf("MaxID: got (%d, %v); want (3, nil)", max, err)
	}

	got, err := store.ReadAfter(ctx, 1, 0)
	if err != nil {
		t.Fatalf("ReadAfter: %v", err)
	}
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("ReadAfter(1): got %d rows starting at %d", len(got), got[0].ID)
	}

	limited, err := store.ReadAfter(ctx, 0, 1)
	if err != nil {
		t.Fatalf("ReadAfter with limit: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != 1 {
		t.Fatalf("ReadAfter limit=1: got %d rows", len(limited))
	}
}

func TestReadByTask(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	mustAppend(t, store, &Event{TaskID: 1, Type: EventTaskCreated, Actor: ActorSystem, Payload: json.RawMessage(`{}`)})
	mustAppend(t, store, &Event{TaskID: 2, Type: EventTaskCreated, Actor: ActorSystem, Payload: json.RawMessage(`{}`)})
	mustAppend(t, store, &Event{TaskID: 1, Type: EventStageEntered, Stage: "setup", Actor: ActorHeartbeat, Payload: json.RawMessage(`{"attempt":1,"reason":"flow","stage_type":"script"}`)})

	events, err := store.ReadByTask(ctx, 1)
	if err != nil {
		t.Fatalf("ReadByTask: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("ReadByTask(1): got %d, want 2", len(events))
	}
	if events[0].Type != EventTaskCreated || events[1].Type != EventStageEntered {
		t.Fatalf("ReadByTask: unexpected order: %+v", events)
	}
}

// fixture builds a small, plausible event log that exercises every
// projection branch we care about: a completed task, a failed task,
// a task waiting on a human, and an active task mid-script.
func fixture(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	mk := func(taskID int64, etype EventType, stage, role string, actor ActorKind, payload string) {
		t.Helper()
		err := store.Append(ctx, &Event{
			TaskID: taskID, Type: etype, Stage: stage, Role: role,
			Actor: actor, Payload: json.RawMessage(payload),
		})
		if err != nil {
			t.Fatalf("fixture append: %v", err)
		}
	}

	// Task 1: completed end-to-end.
	mk(1, EventTaskCreated, "", "", ActorSystem,
		`{"title":"alpha","flow":"default","task_file":"tasks/001-alpha.md","worktree_dir":"/w/1","branch":"task-001"}`)
	mk(1, EventStageEntered, "setup", "", ActorHeartbeat, `{"attempt":1,"reason":"flow","stage_type":"script"}`)
	mk(1, EventStageCompleted, "setup", "", ActorHeartbeat, `{}`)
	mk(1, EventStageEntered, "code", "developer", ActorHeartbeat, `{"attempt":1,"reason":"flow","stage_type":"agent"}`)
	mk(1, EventSessionStarted, "code", "developer", ActorAgent, `{"claude_session_id":"sess-1","pid":1111}`)
	mk(1, EventSessionEnded, "code", "developer", ActorAgent, `{"reason":"completed","exit_code":0}`)
	mk(1, EventStageCompleted, "code", "developer", ActorHeartbeat, `{}`)
	mk(1, EventTaskCompleted, "", "", ActorHeartbeat, `{}`)

	// Task 2: failed at code.
	mk(2, EventTaskCreated, "", "", ActorSystem,
		`{"title":"beta","flow":"default","task_file":"tasks/002-beta.md","worktree_dir":"/w/2","branch":"task-002"}`)
	mk(2, EventStageEntered, "code", "developer", ActorHeartbeat, `{"attempt":1,"reason":"flow","stage_type":"agent"}`)
	mk(2, EventStageFailed, "code", "developer", ActorHeartbeat, `{"reason":"budget_exhausted","last_error":"hooks failed 3x"}`)

	// Task 3: waiting on a human.
	mk(3, EventTaskCreated, "", "", ActorSystem,
		`{"title":"gamma","flow":"default","task_file":"tasks/003-gamma.md","worktree_dir":"/w/3","branch":"task-003"}`)
	mk(3, EventStageEntered, "human_review", "", ActorHeartbeat, `{"attempt":1,"reason":"flow","stage_type":"human"}`)

	// Task 4: mid-script in setup, session ended already (orphan? no, just resumed-test below).
	mk(4, EventTaskCreated, "", "", ActorSystem,
		`{"title":"delta","flow":"default","task_file":"tasks/004-delta.md","worktree_dir":"/w/4","branch":"task-004"}`)
	mk(4, EventStageEntered, "setup", "", ActorHeartbeat, `{"attempt":1,"reason":"flow","stage_type":"script"}`)
}

func TestTasksProjection(t *testing.T) {
	store := newTestStore(t)
	fixture(t, store)

	tasks, err := store.Tasks(context.Background())
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("Tasks: got %d, want 4", len(tasks))
	}

	byID := map[int64]Task{}
	for _, t := range tasks {
		byID[t.ID] = t
	}

	checks := []struct {
		id     int64
		status TaskStatus
		stage  string
	}{
		{1, TaskStatusCompleted, "code"},
		{2, TaskStatusFailed, "code"},
		{3, TaskStatusWaitingHuman, "human_review"},
		{4, TaskStatusActive, "setup"},
	}
	for _, c := range checks {
		got := byID[c.id]
		if got.Status != c.status {
			t.Errorf("task %d: status got %q, want %q", c.id, got.Status, c.status)
		}
		if got.CurrentStage != c.stage {
			t.Errorf("task %d: current_stage got %q, want %q", c.id, got.CurrentStage, c.stage)
		}
	}
}

func TestTaskLookupNotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Task(context.Background(), 999)
	if err == nil || err != ErrNotFound {
		t.Fatalf("Task(999): got %v, want ErrNotFound", err)
	}
}

func TestStageProgressProjection(t *testing.T) {
	store := newTestStore(t)
	fixture(t, store)
	// Add a retry to task 2 so attempts > 1.
	mustAppend(t, store, &Event{
		TaskID: 2, Type: EventStageEntered, Stage: "code", Role: "developer",
		Actor:   ActorHeartbeat,
		Payload: json.RawMessage(`{"attempt":2,"reason":"retry","stage_type":"agent"}`),
	})

	progress, err := store.StageProgressForTask(context.Background(), 2)
	if err != nil {
		t.Fatalf("StageProgressForTask: %v", err)
	}
	if len(progress) != 1 || progress[0].Stage != "code" {
		t.Fatalf("unexpected progress: %+v", progress)
	}
	if progress[0].Attempts != 2 {
		t.Errorf("attempts: got %d, want 2", progress[0].Attempts)
	}
	// The retry's stage.entered came AFTER stage.failed, so the
	// view's status logic resolves to in_progress.
	if progress[0].Status != StageStatusInProgress {
		t.Errorf("status: got %q, want %q", progress[0].Status, StageStatusInProgress)
	}
}

func TestStageAttempts(t *testing.T) {
	store := newTestStore(t)
	fixture(t, store)

	n, err := store.StageAttempts(context.Background(), 1, "code")
	if err != nil {
		t.Fatalf("StageAttempts: %v", err)
	}
	if n != 1 {
		t.Errorf("task 1, code: got %d attempts, want 1", n)
	}

	n, err = store.StageAttempts(context.Background(), 999, "code")
	if err != nil {
		t.Fatalf("StageAttempts(unknown): %v", err)
	}
	if n != 0 {
		t.Errorf("unknown task: got %d, want 0", n)
	}
}

func TestSessionsProjection(t *testing.T) {
	store := newTestStore(t)
	fixture(t, store)

	// Task 1's session ended.
	sess, err := store.Session(context.Background(), 1, "developer")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if sess.ClaudeID != "sess-1" {
		t.Errorf("ClaudeID: got %q, want sess-1", sess.ClaudeID)
	}
	if !sess.Ended {
		t.Errorf("expected ended=true")
	}
	if sess.EndReason != "completed" {
		t.Errorf("EndReason: got %q, want completed", sess.EndReason)
	}
	if sess.LastStage != "code" {
		t.Errorf("LastStage: got %q, want code", sess.LastStage)
	}
}

func TestActiveSessionsAfterResume(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Create + start a session, end it, then resume — active_sessions
	// should include it again because the latest event is a resume.
	mustAppend(t, store, &Event{TaskID: 7, Type: EventTaskCreated, Actor: ActorSystem,
		Payload: json.RawMessage(`{"title":"eta","flow":"default","task_file":"t","worktree_dir":"w","branch":"b"}`)})
	mustAppend(t, store, &Event{TaskID: 7, Type: EventStageEntered, Stage: "code", Role: "dev", Actor: ActorHeartbeat,
		Payload: json.RawMessage(`{"attempt":1,"reason":"flow","stage_type":"agent"}`)})
	mustAppend(t, store, &Event{TaskID: 7, Type: EventSessionStarted, Stage: "code", Role: "dev", Actor: ActorAgent,
		Payload: json.RawMessage(`{"claude_session_id":"sess-7","pid":7777}`)})
	mustAppend(t, store, &Event{TaskID: 7, Type: EventSessionEnded, Stage: "code", Role: "dev", Actor: ActorAgent,
		Payload: json.RawMessage(`{"reason":"completed","exit_code":0}`)})

	active, err := store.ActiveSessions(ctx)
	if err != nil {
		t.Fatalf("ActiveSessions: %v", err)
	}
	for _, s := range active {
		if s.TaskID == 7 {
			t.Fatalf("task 7 should NOT be active after session.ended; got %+v", s)
		}
	}

	mustAppend(t, store, &Event{TaskID: 7, Type: EventSessionResumed, Stage: "code", Role: "dev", Actor: ActorAgent,
		Payload: json.RawMessage(`{"claude_session_id":"sess-7"}`)})

	active, err = store.ActiveSessions(ctx)
	if err != nil {
		t.Fatalf("ActiveSessions after resume: %v", err)
	}
	found := false
	for _, s := range active {
		if s.TaskID == 7 && s.Role == "dev" && s.ClaudeID == "sess-7" && !s.Ended {
			found = true
		}
	}
	if !found {
		t.Fatalf("task 7 should be active after resume; got %+v", active)
	}
}

func TestEnumValuesMatchSchemaContract(t *testing.T) {
	// schema.md / decisions.md fix these literal wire values. If the
	// constant is ever renamed, the SQL views break. Pin them.
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"task.created", string(EventTaskCreated), "task.created"},
		{"stage.entered", string(EventStageEntered), "stage.entered"},
		{"stage.completed", string(EventStageCompleted), "stage.completed"},
		{"stage.failed", string(EventStageFailed), "stage.failed"},
		{"session.started", string(EventSessionStarted), "session.started"},
		{"session.resumed", string(EventSessionResumed), "session.resumed"},
		{"session.ended", string(EventSessionEnded), "session.ended"},
		{"actor.heartbeat", string(ActorHeartbeat), "heartbeat"},
		{"stage_type.human", string(StageHuman), "human"},
		{"task.waiting_human", string(TaskStatusWaitingHuman), "waiting_human"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
}
