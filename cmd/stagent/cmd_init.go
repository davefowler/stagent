package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/davefowler/stagent/scaffold"
	"github.com/spf13/cobra"
)

// gitignoreLines are appended to .gitignore by `stagent init` if
// missing. The three lines are stagent's runtime state.
var gitignoreLines = []string{
	"/.stagent/stagent.db",
	"/.stagent/stagent.db-wal",
	"/.stagent/stagent.db-shm",
	"/.stagent/runner.pid",
	"/.worktrees/",
}

func cmdInit() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Copy the scaffold into the current project",
		Long: `Idempotently writes .stagent.yaml, .stagent/prompts/, .stagent/templates/,
and an empty tasks/ dir. Existing files are not overwritten.

Appends gitignore entries for stagent runtime state if they aren't
already present.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := workingDir(cmd)
			if err != nil {
				return err
			}
			project := filepath.Base(wd)
			return doInit(wd, project, cmd.OutOrStdout())
		},
	}
	return cmd
}

func doInit(targetDir, projectName string, out interface{ Write(p []byte) (int, error) }) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}

	tmpl, err := loadScaffoldTemplate()
	if err != nil {
		return err
	}

	err = fs.WalkDir(scaffold.FS, scaffold.Path, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		// Skip the embed.go file itself — it's a Go source, not a
		// scaffold asset.
		if d.Name() == "embed.go" {
			return nil
		}
		rel := path // scaffold.Path is "." so path is already relative.

		target := filepath.Join(targetDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		if _, err := os.Stat(target); err == nil {
			fmt.Fprintf(out, "skipped: %s (exists)\n", rel)
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}

		raw, err := scaffold.FS.ReadFile(path)
		if err != nil {
			return err
		}

		// .stagent.yaml is templated; the rest copies verbatim.
		body := raw
		if rel == ".stagent.yaml" {
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, map[string]string{"Project": projectName}); err != nil {
				return fmt.Errorf("render scaffold template: %w", err)
			}
			body = buf.Bytes()
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "created: %s\n", rel)
		return nil
	})
	if err != nil {
		return err
	}

	// Ensure tasks/ exists even if scaffold doesn't ship one.
	tasksDir := filepath.Join(targetDir, "tasks")
	if _, err := os.Stat(tasksDir); os.IsNotExist(err) {
		if err := os.MkdirAll(tasksDir, 0o755); err != nil {
			return err
		}
		fmt.Fprintln(out, "created: tasks/")
	}

	// Gitignore append.
	if err := appendGitignore(targetDir, out); err != nil {
		return err
	}
	return nil
}

func loadScaffoldTemplate() (*template.Template, error) {
	b, err := scaffold.FS.ReadFile(".stagent.yaml")
	if err != nil {
		return nil, fmt.Errorf("read scaffold .stagent.yaml: %w", err)
	}
	return template.New("scaffold").Parse(string(b))
}

func appendGitignore(targetDir string, out interface{ Write(p []byte) (int, error) }) error {
	path := filepath.Join(targetDir, ".gitignore")
	existing := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			existing[strings.TrimSpace(sc.Text())] = true
		}
		f.Close()
	}
	var missing []string
	for _, line := range gitignoreLines {
		if !existing[line] {
			missing = append(missing, line)
		}
	}
	if len(missing) == 0 {
		fmt.Fprintln(out, "skipped: .gitignore (entries present)")
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("\n# stagent runtime state\n"); err != nil {
		return err
	}
	for _, line := range missing {
		if _, err := f.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "updated: .gitignore (+%d lines)\n", len(missing))
	return nil
}
