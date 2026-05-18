package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Append inserts an event. The caller fills in Type, TaskID, Stage,
// Role, Actor, and Payload. ID and CreatedAt are assigned by the
// database and written back to the event so the caller can log them.
//
// Append validates the minimum shape (Type and Actor required) but
// otherwise trusts the caller — payload validity is the emitter's
// responsibility. JSON marshaling errors surface as InvalidPayload.
func (s *Store) Append(ctx context.Context, e *Event) error {
	if e == nil {
		return fmt.Errorf("events.Append: nil event")
	}
	if e.Type == "" {
		return fmt.Errorf("events.Append: missing Type")
	}
	if e.Actor == "" {
		return fmt.Errorf("events.Append: missing Actor")
	}

	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	} else if !json.Valid(payload) {
		return fmt.Errorf("events.Append: payload is not valid JSON")
	}

	const q = `
        INSERT INTO events (task_id, type, stage, role, actor, payload)
        VALUES (?, ?, ?, ?, ?, ?)
        RETURNING id, created_at
    `
	var (
		id        int64
		createdAt string
	)
	if err := s.db.QueryRowContext(ctx, q,
		e.TaskID,
		string(e.Type),
		e.Stage,
		e.Role,
		string(e.Actor),
		string(payload),
	).Scan(&id, &createdAt); err != nil {
		return fmt.Errorf("events.Append: insert: %w", err)
	}

	t, err := parseTimestamp(createdAt)
	if err != nil {
		return fmt.Errorf("events.Append: parse created_at %q: %w", createdAt, err)
	}

	e.ID = id
	e.CreatedAt = t
	e.Payload = payload
	return nil
}

// AppendPayload marshals v as JSON and Appends with that payload.
// Convenience wrapper used by the runner and CLI; tests usually go
// through Append directly to assert on the raw bytes.
func (s *Store) AppendPayload(ctx context.Context, e *Event, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("events.AppendPayload: marshal: %w", err)
	}
	e.Payload = b
	return s.Append(ctx, e)
}

// parseTimestamp accepts the strftime format we set as the column
// default ("YYYY-MM-DDTHH:MM:SS.fffZ") and falls back to RFC3339Nano
// for any future-proofed callers writing their own timestamp.
func parseTimestamp(s string) (time.Time, error) {
	const sqliteFmt = "2006-01-02T15:04:05.000Z"
	if t, err := time.Parse(sqliteFmt, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
