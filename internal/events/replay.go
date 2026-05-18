package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// ReadAfter returns events with id > cursor, in ascending id order.
// Pass cursor = 0 for the full replay. limit ≤ 0 means no limit.
//
// This is the primary read path: the runner replays from cursor 0 on
// startup to rebuild in-memory state, then advances the cursor as new
// events stream in. The viewer uses the same pattern.
func (s *Store) ReadAfter(ctx context.Context, cursor int64, limit int) ([]Event, error) {
	q := `SELECT id, task_id, type, stage, role, actor, payload, created_at
          FROM events
          WHERE id > ?
          ORDER BY id ASC`
	args := []any{cursor}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("events.ReadAfter: query: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReadByTask returns every event for one task, oldest first. Used by
// `stagent log <id>` and by the runner's per-task replay path.
func (s *Store) ReadByTask(ctx context.Context, taskID int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, task_id, type, stage, role, actor, payload, created_at
         FROM events
         WHERE task_id = ?
         ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("events.ReadByTask: query: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MaxID returns the highest event id currently in the log, or 0 if
// the log is empty. Cheap (single index lookup). The runner uses
// this to initialize the cursor when it starts up.
func (s *Store) MaxID(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("events.MaxID: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}

func scanEvent(rows *sql.Rows) (Event, error) {
	var (
		e            Event
		etype, actor string
		payload      string
		createdAt    string
	)
	if err := rows.Scan(&e.ID, &e.TaskID, &etype, &e.Stage, &e.Role, &actor, &payload, &createdAt); err != nil {
		return Event{}, fmt.Errorf("events: scan: %w", err)
	}
	e.Type = EventType(etype)
	e.Actor = ActorKind(actor)
	e.Payload = json.RawMessage(payload)
	t, err := parseTimestamp(createdAt)
	if err != nil {
		return Event{}, fmt.Errorf("events: parse created_at %q: %w", createdAt, err)
	}
	e.CreatedAt = t
	return e, nil
}
