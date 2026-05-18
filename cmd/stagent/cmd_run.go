package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/davefowler/stagent/internal/hooks"
	"github.com/davefowler/stagent/internal/runner"
	"github.com/spf13/cobra"
)

func cmdRun() *cobra.Command {
	var claudeBin string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the heartbeat loop in the foreground",
		Long: `Loads .stagent.yaml, opens the event log, runs crash recovery,
and drives any active tasks through their flow. Blocks until
Ctrl-C or SIGTERM.

Per notes/architecture.md: foreground-only in v0.1. To run in the
background, use tmux/screen/nohup or your OS's service manager.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			wd, err := workingDir(cmd)
			if err != nil {
				return err
			}
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			store, err := openStore(wd)
			if err != nil {
				return err
			}
			defer store.Close()

			cfgPath, err := configPath(cmd)
			if err != nil {
				return err
			}

			r, err := runner.New(runner.Options{
				WorkingDir: wd,
				Store:      store,
				ConfigPath: cfgPath,
				Config:     cfg,
				Registry:   hooks.NewDefault(),
				ClaudeBin:  claudeBin,
			})
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(),
				syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			fmt.Fprintln(cmd.OutOrStdout(), "stagent run — Ctrl-C to stop")
			return r.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&claudeBin, "claude-bin", "",
		"path to the claude binary (default: $STAGENT_CLAUDE_BIN or `claude`)")
	return cmd
}
