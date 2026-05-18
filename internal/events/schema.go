package events

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// Store is the durable event log. One process per DB; concurrent
// readers (viewer, additional CLI invocations) work via WAL.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens (and if needed creates) the SQLite database at path.
// The events table, append-only triggers, and all projection views
// are applied unconditionally — views are dropped and recreated so
// schema-changes-via-view-rewrite are cheap (notes/schema.md).
//
// Pass ":memory:" for an ephemeral database used in tests.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := buildDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	// Apply DDL on a single connection so the PRAGMA settings stick
	// for at least the schema steps; subsequent connections from the
	// pool inherit journal_mode (it's persistent) but pick up
	// synchronous separately, which we apply via the DSN.
	if err := applySchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db, path: path}, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw *sql.DB for tests and for the runner's
// recovery path which needs ad-hoc queries. Production code should
// prefer the higher-level methods.
func (s *Store) DB() *sql.DB { return s.db }

// buildDSN constructs a modernc.org/sqlite DSN. PRAGMAs go through
// the `_pragma` query parameter — these run on every connection the
// pool hands out, which matters for `synchronous` (per-connection)
// even though `journal_mode=WAL` is persistent.
func buildDSN(path string) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(5000)")
	if path == ":memory:" {
		return "file::memory:?cache=shared&" + q.Encode()
	}
	return "file:" + path + "?" + q.Encode()
}

// applySchema runs the DDL. Idempotent: the events table uses CREATE
// TABLE IF NOT EXISTS; views and triggers are dropped + recreated.
func applySchema(ctx context.Context, db *sql.DB) error {
	for _, stmt := range schemaStatements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema: %w\n  stmt: %s", err, stmt)
		}
	}
	return nil
}

// schemaStatements is the full DDL, in execution order. Kept as a
// slice of statements (not one blob) so the driver doesn't have to
// split on `;` and individual errors point at a single statement.
var schemaStatements = []string{
	// ─── Table ────────────────────────────────────────────────────
	`CREATE TABLE IF NOT EXISTS events (
        id          INTEGER PRIMARY KEY AUTOINCREMENT,
        task_id     INTEGER NOT NULL,
        type        TEXT    NOT NULL,
        stage       TEXT    NOT NULL DEFAULT '',
        role        TEXT    NOT NULL DEFAULT '',
        actor       TEXT    NOT NULL,
        payload     TEXT    NOT NULL DEFAULT '{}',
        created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
    ) STRICT`,

	`CREATE INDEX IF NOT EXISTS idx_events_task        ON events(task_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_events_type        ON events(type, id)`,
	`CREATE INDEX IF NOT EXISTS idx_events_task_stage  ON events(task_id, stage, id)`,

	// ─── Append-only triggers ─────────────────────────────────────
	`DROP TRIGGER IF EXISTS events_no_update`,
	`CREATE TRIGGER events_no_update BEFORE UPDATE ON events
        BEGIN SELECT RAISE(ABORT, 'events are append-only'); END`,

	`DROP TRIGGER IF EXISTS events_no_delete`,
	`CREATE TRIGGER events_no_delete BEFORE DELETE ON events
        BEGIN SELECT RAISE(ABORT, 'events are append-only'); END`,

	// ─── Views (dropped + recreated) ──────────────────────────────
	`DROP VIEW IF EXISTS active_sessions`,
	`DROP VIEW IF EXISTS sessions`,
	`DROP VIEW IF EXISTS stage_progress`,
	`DROP VIEW IF EXISTS tasks`,

	`CREATE VIEW tasks AS
        WITH
          created AS (
            SELECT
                task_id,
                json_extract(payload, '$.title')        AS title,
                json_extract(payload, '$.flow')         AS flow,
                json_extract(payload, '$.task_file')    AS task_file,
                json_extract(payload, '$.worktree_dir') AS worktree_dir,
                json_extract(payload, '$.branch')       AS branch,
                created_at                              AS created_at
            FROM events WHERE type = 'task.created'
          ),
          latest_stage AS (
            SELECT task_id, MAX(id) AS evt_id
            FROM events
            WHERE type IN ('stage.entered', 'stage.completed', 'stage.failed')
            GROUP BY task_id
          ),
          latest_stage_event AS (
            SELECT
                e.task_id,
                e.stage,
                e.type,
                e.created_at,
                json_extract(e.payload, '$.stage_type') AS stage_type
            FROM events e
            JOIN latest_stage ls ON e.id = ls.evt_id
          ),
          terminal AS (
            SELECT task_id, type, created_at
            FROM events
            WHERE type IN ('task.completed', 'task.aborted')
          )
        SELECT
            c.task_id                                          AS id,
            c.title,
            c.flow,
            c.task_file,
            c.worktree_dir,
            c.branch,
            COALESCE(lse.stage, '')                            AS current_stage,
            CASE
                WHEN t.type = 'task.completed'       THEN 'completed'
                WHEN t.type = 'task.aborted'         THEN 'aborted'
                WHEN lse.type = 'stage.failed'       THEN 'failed'
                WHEN lse.type = 'stage.entered'
                 AND lse.stage_type = 'human'        THEN 'waiting_human'
                ELSE 'active'
            END                                                AS status,
            c.created_at,
            COALESCE(lse.created_at, c.created_at)             AS updated_at
        FROM created c
        LEFT JOIN latest_stage_event lse ON lse.task_id = c.task_id
        LEFT JOIN terminal           t   ON t.task_id   = c.task_id`,

	`CREATE VIEW stage_progress AS
        SELECT
            task_id,
            stage,
            SUM(CASE WHEN type = 'stage.entered' THEN 1 ELSE 0 END) AS attempts,
            CASE
                WHEN MAX(CASE WHEN type = 'stage.completed' THEN id END) >
                     COALESCE(MAX(CASE WHEN type = 'stage.entered' THEN id END), 0)
                THEN 'completed'
                WHEN MAX(CASE WHEN type = 'stage.failed' THEN id END) >
                     COALESCE(MAX(CASE WHEN type = 'stage.entered' THEN id END), 0)
                THEN 'failed'
                WHEN MAX(CASE WHEN type = 'stage.entered' THEN id END) IS NOT NULL
                THEN 'in_progress'
                ELSE 'not_started'
            END AS status,
            MAX(created_at) AS last_event_at
        FROM events
        WHERE stage != ''
        GROUP BY task_id, stage`,

	// sessions: latest session.* event per (task, role), with the
	// claude_session_id pulled from the most recent start/resume
	// (session.ended payloads don't carry the id).
	`CREATE VIEW sessions AS
        WITH se AS (
            SELECT
                task_id, role, stage, id, type, created_at, payload
            FROM events
            WHERE type IN ('session.started', 'session.resumed', 'session.ended')
              AND role != ''
        ),
        latest AS (
            SELECT task_id, role, MAX(id) AS evt_id
            FROM se GROUP BY task_id, role
        ),
        latest_id AS (
            SELECT
                task_id, role,
                MAX(CASE WHEN type IN ('session.started', 'session.resumed') THEN id END) AS id_evt_id
            FROM se GROUP BY task_id, role
        )
        SELECT
            l.task_id,
            l.role,
            (SELECT json_extract(payload, '$.claude_session_id')
               FROM events WHERE id = li.id_evt_id) AS claude_id,
            le.stage         AS last_stage,
            le.created_at    AS last_used_at,
            (le.type = 'session.ended') AS ended,
            CASE WHEN le.type = 'session.ended'
                 THEN json_extract(le.payload, '$.reason')
                 ELSE NULL
            END AS end_reason
        FROM latest l
        JOIN se      le ON le.id = l.evt_id
        JOIN latest_id li ON li.task_id = l.task_id AND li.role = l.role`,

	`CREATE VIEW active_sessions AS
        SELECT * FROM sessions WHERE ended = 0`,
}
