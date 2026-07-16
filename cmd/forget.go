package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jclement/restic-duper/internal/config"
	"github.com/jclement/restic-duper/internal/runner"
)

var (
	flagForgetPairs  []string
	flagForgetDryRun bool
	flagNoPrune      bool
)

var forgetCmd = &cobra.Command{
	Use:   "forget",
	Short: "Apply each pair's retention policy to its destination repository",
	Long: `forget runs "restic forget --prune" against the DESTINATION repository of
every pair that has a retention block, applying its keep policy and
reclaiming space. Source repositories are never touched — their retention
belongs to whatever creates their backups.

Pairs without a retention block are skipped. Intended to be scheduled less
often than "run" (e.g. weekly); note that prune takes exclusive repository
locks, so schedule it when no copy is running.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		log := newLogger()
		var rend *progressRenderer
		if !flagForgetDryRun && useProgress(os.Stderr) {
			rend = newProgressRenderer(os.Stderr, useColor())
			defer rend.Close()
			level := slog.LevelWarn
			if flagVerbose {
				level = slog.LevelDebug
			}
			log = slog.New(newConsoleHandler(rend.LogWriter(), level))
		}
		path, err := configPath()
		if err != nil {
			return err
		}
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		warnConfigPerms(log, path)

		pairs, err := selectPairs(cfg, flagForgetPairs)
		if err != nil {
			return err
		}
		for _, n := range flagForgetPairs {
			for i := range pairs {
				if pairs[i].Name == n && pairs[i].Retention == nil {
					return fmt.Errorf("pair %q has no retention block", n)
				}
			}
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		r := &runner.Runner{Restic: cfg.ResticBinary, Log: log, Verbose: flagVerbose}
		if r.Restic == "" {
			r.Restic = "restic"
		}
		if rend != nil {
			r.Progress = rend.Event
		}
		if err := r.CheckRestic(ctx); err != nil {
			return err
		}

		started := time.Now()
		var results []runner.Result
		failed, skipped := 0, 0
		for i := range pairs {
			if ctx.Err() != nil {
				log.Warn("interrupted; skipping remaining pairs", "remaining", len(pairs)-i)
				break
			}
			if pairs[i].Retention == nil {
				log.Info("no retention policy; skipping", "pair", pairs[i].Name)
				skipped++
				continue
			}
			if rend != nil {
				rend.StartPair(fmt.Sprintf("[%d/%d] forget %s  %s", i+1, len(pairs),
					pairs[i].Name, truncate(runner.RedactRepo(pairs[i].To.Repo), 40)))
			}
			res := r.ForgetPair(ctx, &pairs[i], !flagNoPrune, flagForgetDryRun)
			results = append(results, res)
			if !res.OK() {
				failed++
			}
			if rend != nil {
				detail := "policy applied"
				if !res.OK() {
					detail = res.Error
				}
				rend.FinishPair(res.OK(), detail, res.Duration)
			}
		}

		if !flagForgetDryRun {
			sendNotification(log, cfg, "forget", started, results)
		}

		if rend != nil {
			rend.Summary(failed == 0, fmt.Sprintf("%d succeeded, %d failed, %d skipped  (%s)",
				len(results)-failed, failed, skipped, time.Since(started).Round(time.Second)))
		} else {
			summary := log.With("succeeded", len(results)-failed, "failed", failed, "skipped", skipped,
				"duration", time.Since(started).Round(time.Second))
			if failed > 0 {
				summary.Error("forget finished with failures")
			} else if ctx.Err() == nil {
				summary.Info("forget finished")
			}
		}
		if failed > 0 {
			os.Exit(2)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("forget interrupted")
		}
		return nil
	},
}

func init() {
	forgetCmd.Flags().StringSliceVar(&flagForgetPairs, "pair", nil, "only the named pair(s); repeatable")
	forgetCmd.Flags().BoolVar(&flagForgetDryRun, "dry-run", false, "show what would be forgotten without changing anything")
	forgetCmd.Flags().BoolVar(&flagNoPrune, "no-prune", false, "forget snapshots but skip pruning unreferenced data")
	rootCmd.AddCommand(forgetCmd)
}
