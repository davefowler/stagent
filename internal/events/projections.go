package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned by singular projection lookups (Task, etc.)
// when no row matches.
var ErrNotFound = errors.New("not found")

// Task is the projected current state of a task. Read from the
// `tasks` view defined in schema.go.
type Task struct {
	ID           int64
	Title        string
	Flow         string
	TaskFile     string
	WorktreeDir  string
	Branch       string
	CurrentStage string
	Status       TaskStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Session is the projected latest Claude session for one (task, role).
type Session struct {
	TaskID     int64
	Role       string
	ClaudeID   string
	LastStage  string
	LastUsedAt time.Time
	Ended      bool
	EndReason  string
}

// StageProgress is the projected attempt count and status for one
// (task, stage) pair.
type StageProgress struct {
	TaskID      int64
	Stage       string
	Attempts    int
	Status      StageStatus
	LastEventAt time.Time
}

// Tasks returns every task in the log, oldest first by id.
func (s *Store) Tasks(ctx context.Context) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, title, flow, task_file, worktree_dir, branch,
               current_stage, status, created_at, updated_at
        FROM tasks
        ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("events.Tasks: %w", err)
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Task returns the one task with the given id, or ErrNotFound.
func (s *Store) Task(ctx context.Context, id int64) (Task, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, title, flow, task_file, worktree_dir, branch,
               current_stage, status, created_at, updated_at
        FROM tasks
        WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	return t, err
}

// SessionsForTask returns every (task, role) session row for one
// task. Used by `stagent show` and the runner's per-task dispatcher.
func (s *Store) SessionsForTask(ctx context.Context, taskID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT task_id, role,
               COALESCE(claude_id, ''),
               COALESCE(last_stage, ''),
               last_used_at,
               ended,
               COALESCE(end_reason, '')
        FROM sessions
        WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, fmt.Errorf("events.SessionsForTask: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// Session returns the projected session for one (task, role), or
// ErrNotFound.
func (s *Store) Session(ctx context.Context, taskID int64, role string) (Session, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT task_id, role,
               COALESCE(claude_id, ''),
               COALESCE(last_stage, ''),
               last_used_at,
               ended,
               COALESCE(end_reason, '')
        FROM sessions
        WHERE task_id = ? AND role = ?`, taskID, role)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return sess, err
}

// ActiveSessions returns every (task, role) pair whose session has a
// start/resume newer than its end (or no end at all). The runner
// uses this on startup to find orphans from a previous crash
// (architecture.md "Crash recovery").
func (s *Store) ActiveSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT task_id, role,
               COALESCE(claude_id, ''),
               COALESCE(last_stage, ''),
               last_used_at,
               ended,
               COALESCE(end_reason, '')
        FROM active_sessions`)
	if err != nil {
		return nil, fmt.Errorf("events.ActiveSessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// StageProgressForTask returns one row per (task, stage) the task
// has interacted with.
func (s *Store) StageProgressForTask(ctx context.Context, taskID int64) ([]StageProgress, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT task_id, stage, attempts, status, last_event_at
        FROM stage_progress
        WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, fmt.Errorf("events.StageProgressForTask: %w", err)
	}
	defer rows.Close()
	var out []StageProgress
	for rows.Next() {
		p, err := scanStageProgress(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// StageAttempts returns just the attempt count for (task, stage).
// Zero if the stage has never been entered. This is the hot path
// for max_runs checks — the runner calls it on every dispatch
// decision, so it stays as a direct query rather than reusing
// StageProgressForTask.
func (s *Store) StageAttempts(ctx context.Context, taskID int64, stage string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
        SELECT COUNT(*) FROM events
        WHERE task_id = ? AND stage = ? AND type = 'stage.entered'`,
		taskID, stage).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("events.StageAttempts: %w", err)
	}
	return n, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(r rowScanner) (Task, error) {
	var (
		t                 Task
		status            string
		createdAt, updAt  string
	)
	if err := r.Scan(&t.ID, &t.Title, &t.Flow, &t.TaskFile, &t.WorktreeDir, &t.Branch,
		&t.CurrentStage, &status, &createdAt, &updAt); err != nil {
		return Task{}, err
	}
	t.Status = TaskStatus(status)
	var err error
	if t.CreatedAt, err = parseTimestamp(createdAt); err != nil {
		return Task{}, fmt.Errorf("events: parse task.created_at %q: %w", createdAt, err)
	}
	if t.UpdatedAt, err = parseTimestamp(updAt); err != nil {
		return Task{}, fmt.Errorf("events: parse task.updated_at %q: %w", updAt, err)
	}
	return t, nil
}

func scanSession(r rowScanner) (Session, error) {
	var (
		sess     Session
		lastUsed string
		endedInt int
	)
	if err := r.Scan(&sess.TaskID, &sess.Role, &sess.ClaudeID, &sess.LastStage,
		&lastUsed, &endedInt, &sess.EndReason); err != nil {
		return Session{}, err
	}
	sess.Ended = endedInt != 0
	t, err := parseTimestamp(lastUsed)
	if err != nil {
		return Session{}, fmt.Errorf("events: parse session.last_used_at %q: %w", lastUsed, err)
	}
	sess.LastUsedAt = t
	return sess, nil
}

func scanStageProgress(r rowScanner) (StageProgress, error) {
	var (
		p         StageProgress
		status    string
		lastEvent string
	)
	if err := r.Scan(&p.TaskID, &p.Stage, &p.Attempts, &status, &lastEvent); err != nil {
		return StageProgress{}, err
	}
	p.Status = StageStatus(status)
	t, err := parseTimestamp(lastEvent)
	if err != nil {
		return StageProgress{}, fmt.Errorf("events: parse stage_progress.last_event_at %q: %w", lastEvent, err)
	}
	p.LastEventAt = t
	return p, nil
}
