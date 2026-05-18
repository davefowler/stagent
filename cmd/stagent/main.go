// stagent is the CLI for the stagent workflow runner.
//
// See `stagent --help` for the full subcommand list and
// notes/architecture.md "Commands" for the rationale.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is overwritten at link time for tagged releases (see
// the release workflow, when one exists).
var version = "0.1.0-dev"

func main() {
	if err := root().Execute(); err != nil {
		// Cobra already printed the error.
		os.Exit(1)
	}
}

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "stagent",
		Short:         "Stage-based workflow runner for AI agents",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	cmd.PersistentFlags().StringP("working-dir", "C", "", "project root (default: cwd)")
	cmd.PersistentFlags().String("config", "", "path to .stagent.yaml (default: <working-dir>/.stagent.yaml)")

	cmd.AddCommand(
		cmdInit(),
		cmdNew(),
		cmdRun(),
		cmdList(),
		cmdStatus(),
		cmdShow(),
		cmdLog(),
		cmdGoto(),
		cmdAbort(),
		cmdSession(),
		cmdRestart(),
	)
	return cmd
}

// workingDir returns the project root either from --working-dir or
// the process cwd.
func workingDir(cmd *cobra.Command) (string, error) {
	v, _ := cmd.Flags().GetString("working-dir")
	if v != "" {
		return v, nil
	}
	return os.Getwd()
}

// configPath returns the resolved path to .stagent.yaml.
func configPath(cmd *cobra.Command) (string, error) {
	v, _ := cmd.Flags().GetString("config")
	if v != "" {
		return v, nil
	}
	wd, err := workingDir(cmd)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/.stagent.yaml", wd), nil
}
