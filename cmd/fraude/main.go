// fraude is a mock `claude` binary for stagent's test suite.
//
// It accepts the same flags as the real Claude CLI in headless
// (-p) mode, reads scripted responses from $STAGENT_FAKE_RESPONSES,
// and writes a JSONL transcript to
// ~/.claude/projects/<encoded-cwd>/<session-id>.jsonl — matching the
// path real Claude uses, so the runner's --resume logic works
// against fraude without special-casing.
//
// See notes/decisions.md §5 for the contract.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Response is one scripted assistant turn.
type Response struct {
	// Text is what the assistant "says". Printed to stdout (real
	// claude does the same in headless mode) and recorded in the
	// JSONL.
	Text string `json:"text"`

	// ExitCode overrides the default exit (0). Used to simulate
	// agent crashes, token-limit terminations, etc.
	ExitCode int `json:"exit_code,omitempty"`

	// DelayMs sleeps before exit. Simulates slow agents so tests
	// can exercise the runner's wait-for-exit behavior.
	DelayMs int `json:"delay_ms,omitempty"`

	// FileOps are applied after the delay but before exit. Each
	// op edits one file on disk, so tests can assert that hooks
	// see a realistic post-condition (boxes ticked, files written).
	FileOps []FileOp `json:"file_ops,omitempty"`
}

// FileOp is one scripted file edit. Exactly one of Write/Append/
// Replace must be set; mixing is a config error.
type FileOp struct {
	Path    string    `json:"path"`
	Write   string    `json:"write,omitempty"`   // overwrite file with this content
	Append  string    `json:"append,omitempty"`  // append to file
	Replace []ReplacePair `json:"replace,omitempty"` // string replacements
}

type ReplacePair struct {
	Find string `json:"find"`
	With string `json:"with"`
}

func main() {
	var (
		prompt        string
		sessionID     string
		resume        string
		systemPrompt  string
		dangerously   bool
		help          bool
	)

	fs := flag.NewFlagSet("fraude", flag.ContinueOnError)
	fs.StringVar(&prompt, "p", "", "headless prompt")
	fs.StringVar(&sessionID, "session-id", "", "session UUID for first invocation")
	fs.StringVar(&resume, "resume", "", "session UUID to resume")
	fs.StringVar(&systemPrompt, "system-prompt", "", "system prompt (or @path)")
	fs.BoolVar(&dangerously, "dangerously-skip-permissions", false, "accepted as no-op")
	fs.BoolVar(&help, "help", false, "show help")

	// Real claude accepts --no-session-persistence among many others;
	// ContinueOnError lets us ignore unknown flags rather than crash.
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(os.Args[1:]); err != nil {
		// flag.ContinueOnError still prints the error itself.
		os.Exit(2)
	}
	if help {
		fs.Usage()
		return
	}

	if prompt == "" {
		fmt.Fprintln(os.Stderr, "fraude: -p prompt is required")
		os.Exit(2)
	}
	if sessionID == "" && resume == "" {
		fmt.Fprintln(os.Stderr, "fraude: --session-id or --resume is required")
		os.Exit(2)
	}

	uuid := sessionID
	if uuid == "" {
		uuid = resume
	}

	resp, err := pickResponse(prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fraude: %v\n", err)
		os.Exit(2)
	}

	transcript, err := openTranscript(uuid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fraude: %v\n", err)
		os.Exit(2)
	}
	defer transcript.Close()

	if sessionID != "" {
		writeJSONL(transcript, map[string]any{
			"type":           "system",
			"session_id":     sessionID,
			"system_prompt":  resolveSystemPrompt(systemPrompt),
			"cwd":            mustCWD(),
			"started_at":     time.Now().UTC().Format(time.RFC3339Nano),
		})
	} else {
		writeJSONL(transcript, map[string]any{
			"type":       "resume",
			"session_id": resume,
			"at":         time.Now().UTC().Format(time.RFC3339Nano),
		})
	}

	writeJSONL(transcript, map[string]any{
		"type":    "user",
		"content": prompt,
	})

	// Apply scripted file ops before "speaking" — matches the real
	// pattern where the assistant uses tools (edits files) and then
	// produces a summary message.
	for _, op := range resp.FileOps {
		if err := applyFileOp(op); err != nil {
			fmt.Fprintf(os.Stderr, "fraude: file_op %s: %v\n", op.Path, err)
			os.Exit(2)
		}
		writeJSONL(transcript, map[string]any{
			"type": "tool_use",
			"tool": "edit",
			"path": op.Path,
		})
	}

	writeJSONL(transcript, map[string]any{
		"type":    "assistant",
		"content": resp.Text,
	})

	// stdout: real claude prints the assistant message in -p mode.
	fmt.Println(resp.Text)

	if resp.DelayMs > 0 {
		time.Sleep(time.Duration(resp.DelayMs) * time.Millisecond)
	}

	if resp.ExitCode != 0 {
		os.Exit(resp.ExitCode)
	}
}

// pickResponse reads the scripted responses pointed at by
// $STAGENT_FAKE_RESPONSES. Supports two shapes:
//   - JSON array: queued; each invocation pops next via an on-disk
//     cursor file alongside the responses file.
//   - JSON object: prompt-prefix map; first key that's a substring
//     prefix of the prompt wins.
func pickResponse(prompt string) (Response, error) {
	path := os.Getenv("STAGENT_FAKE_RESPONSES")
	if path == "" {
		// No script → trivial "OK" response so the binary is still
		// useful as a smoke test.
		return Response{Text: "(fraude: no STAGENT_FAKE_RESPONSES set)"}, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return Response{}, fmt.Errorf("read %s: %w", path, err)
	}

	// Try array first; fall back to map.
	var queue []Response
	if err := json.Unmarshal(raw, &queue); err == nil {
		return popQueue(path, queue)
	}

	var byPrefix map[string]Response
	if err := json.Unmarshal(raw, &byPrefix); err == nil {
		for prefix, resp := range byPrefix {
			if strings.Contains(prompt, prefix) {
				return resp, nil
			}
		}
		return Response{}, fmt.Errorf("no prompt-prefix matched prompt: %q", truncate(prompt, 120))
	}

	return Response{}, fmt.Errorf("%s is neither a JSON array nor an object", path)
}

// popQueue returns queue[cursor] and advances the cursor on disk
// (written next to the responses file as `<path>.cursor`). When the
// queue is exhausted, returns an error so the test fails clearly
// instead of looping.
func popQueue(path string, queue []Response) (Response, error) {
	cursorPath := path + ".cursor"
	cursor := 0
	if b, err := os.ReadFile(cursorPath); err == nil {
		fmt.Sscanf(string(b), "%d", &cursor)
	}
	if cursor >= len(queue) {
		return Response{}, fmt.Errorf("response queue exhausted (%d items, cursor=%d)",
			len(queue), cursor)
	}
	r := queue[cursor]
	if err := os.WriteFile(cursorPath, []byte(fmt.Sprintf("%d", cursor+1)), 0o644); err != nil {
		return Response{}, fmt.Errorf("advance cursor: %w", err)
	}
	return r, nil
}

func resolveSystemPrompt(s string) string {
	if !strings.HasPrefix(s, "@") {
		return s
	}
	b, err := os.ReadFile(s[1:])
	if err != nil {
		return fmt.Sprintf("<failed to read %s: %v>", s[1:], err)
	}
	return string(b)
}

func applyFileOp(op FileOp) error {
	if op.Path == "" {
		return fmt.Errorf("file_op missing path")
	}
	set := 0
	if op.Write != "" {
		set++
	}
	if op.Append != "" {
		set++
	}
	if len(op.Replace) > 0 {
		set++
	}
	if set != 1 {
		return fmt.Errorf("file_op must set exactly one of write/append/replace (got %d)", set)
	}
	switch {
	case op.Write != "":
		return os.WriteFile(op.Path, []byte(op.Write), 0o644)
	case op.Append != "":
		f, err := os.OpenFile(op.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = f.WriteString(op.Append)
		return err
	default:
		b, err := os.ReadFile(op.Path)
		if err != nil {
			return err
		}
		s := string(b)
		for _, rp := range op.Replace {
			s = strings.ReplaceAll(s, rp.Find, rp.With)
		}
		return os.WriteFile(op.Path, []byte(s), 0o644)
	}
}

// openTranscript opens (creating dirs as needed) the JSONL file
// matching real Claude's path convention:
//   ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl
// where encoded-cwd substitutes `-` for `/`. Append mode so
// --resume picks up where the prior turn left off.
func openTranscript(uuid string) (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	encoded := strings.ReplaceAll(cwd, "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, uuid+".jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

func writeJSONL(w *os.File, v map[string]any) {
	b, _ := json.Marshal(v)
	w.Write(b)
	w.Write([]byte("\n"))
}

func mustCWD() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "<unknown>"
	}
	return cwd
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
