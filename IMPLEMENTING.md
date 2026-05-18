# For the implementing agent

This repo was specified before any Go code was written. Your job is to
implement **v0.1** from the locked decisions in `notes/decisions.md`.

## Read in order (under 90 minutes)

1. **`notes/decisions.md`** — the contract. Locked decisions are
   non-negotiable without user approval. Tabled items are out of scope
   for v0.1.
2. **`notes/architecture.md`** — types, lifecycle, design rationale.
3. **`notes/schema.md`** — SQLite schema, event types, views.
4. **`notes/config.md`** — `.stagent.yaml` reference.

User-facing docs (`docs/`) cover the same material rendered for end
users. Read them only when `notes/` has a gap.

## v0.1 scope (decisions.md section 6)

**In:**

- Event log + schema + views + append-only triggers (WAL).
- Runner: heartbeat + task workers, PID file, SIGHUP reload, crash
  recovery via orphan-session detection.
- Stage types: `agent`, `script`.
- Hook slots: `enter`, `exit`.
- Hooks: `run_shell`, `section_check`, `min_words`,
  `validate_task_sections`.
- CLI: `init`, `new`, `run`, `status`, `list`, `show`, `log`, `goto`,
  `abort`, `session`, `restart`.
- Default flow: `setup → code → cleanup` (the scaffold's slim flow).
- Section-path parser: literal segments + regex `/.../[N]`.
- `cmd/fraude/` mock-claude binary for tests.

**Out (deferred to v0.2 / v0.3):**

- `human` stage type. `tick` hooks. `wait_for_*` / `ci_status`.
- `pr`, `review`, `human_review` stages in the default flow.
- SwiftUI viewer. `commands:` user recipes. `force_tick` /
  `stagent poll`.

## Build order suggestion

1. `go.mod` is done; layout per `notes/decisions.md` section 12.
2. `internal/events/` — schema, append, replay. Unit-tested with raw SQL.
3. `internal/sections/` — markdown path parser + matcher. Pure logic;
   easy to test exhaustively.
4. `internal/config/` — YAML load, struct validation.
5. `internal/hooks/` — interface + the four v0.1 hooks.
6. `internal/runner/` — heartbeat, task worker, `claude` subprocess.
7. `cmd/fraude/` — mock claude (decisions.md section 5).
8. `cmd/stagent/` — CLI subcommands via cobra.
9. End-to-end test: `stagent init` → write task → `stagent run` → task
   completes (using fraude scripted responses).

## House rules

- Libraries per decisions.md section 13. Don't reach for alternatives.
- Go 1.22+ (decisions.md section 14).
- Tests live alongside the code (`foo_test.go` next to `foo.go`).
- `mkdocs build --strict` passes on every PR.
- Anti-slop conventions per
  `scaffold/.stagent/prompts/roles/developer.md`.
- New decisions go in `notes/decisions.md` with a changelog entry.
  No silent design drift.

## When stuck

Re-read `notes/decisions.md`. If your question still isn't answered:

- If it's in "Tabled" — it's out of scope; ship without it.
- If it's a genuine new question — ask the user. Don't invent.

## Local dev

```bash
go test ./...                  # unit tests
go build ./cmd/stagent         # build the main binary
go build ./cmd/fraude          # build the mock claude
mkdocs serve                   # preview docs at localhost:8000
```

CI runs `go vet`, `go test`, and `mkdocs build --strict` on every
push and PR.
