package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jclement/restic-duper/internal/config"
	"github.com/jclement/restic-duper/internal/notify"
	"github.com/jclement/restic-duper/internal/runner"
)

var (
	flagDryRun bool
	flagPairs  []string
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Copy snapshots for every configured pair",
	Args:  cobra.NoArgs,
	RunE:  runRun,
}

func init() {
	runCmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "print the restic commands without executing them")
	runCmd.Flags().StringSliceVar(&flagPairs, "pair", nil, "only run the named pair(s); repeatable")
	rootCmd.AddCommand(runCmd)
}

func runRun(cmd *cobra.Command, _ []string) error {
	log := newLogger()
	// On an interactive terminal, render live progress and route logs
	// through the renderer; warnings and errors still show, routine info
	// is replaced by the rendered lines. With -v, raw restic output prints
	// as dim lines above the live line.
	var rend *progressRenderer
	if !flagDryRun && useProgress() {
		rend = newProgressRenderer(os.Stderr, useColor())
		defer rend.Close()
		w := rend.LogWriter()
		log = slog.New(newConsoleHandler(w, w, slog.LevelWarn))
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
	started := time.Now()

	// Once the config (and thus the webhook) is known, setup failures are
	// reported through it too — otherwise webhook-only monitoring would
	// never hear about a bad pair name or a missing restic binary.
	setupFail := func(err error) error {
		if !flagDryRun && cfg.Notifications.Webhook != nil && cfg.Notifications.Webhook.FireOnFailure() {
			p := notify.NewPayload(version, started, nil)
			p.Command = "run"
			p.Status = "failure"
			p.Error = err.Error()
			nctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if nerr := notify.Send(nctx, log, cfg.Notifications.Webhook, p); nerr != nil {
				log.Error("notification failed", "error", nerr)
			}
		}
		return err
	}

	pairs, err := selectPairs(cfg, flagPairs)
	if err != nil {
		return setupFail(err)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := &runner.Runner{
		Restic:  cfg.ResticBinary,
		Log:     log,
		DryRun:  flagDryRun,
		Verbose: flagVerbose,
	}
	if r.Restic == "" {
		r.Restic = "restic"
	}
	if rend != nil {
		r.Progress = rend.Event
		if flagVerbose {
			r.Output = rend.VerboseLine
		}
	}
	if !flagDryRun {
		if err := r.CheckRestic(ctx); err != nil {
			return setupFail(err)
		}
	}
	var results []runner.Result
	failed := 0
	for i := range pairs {
		if ctx.Err() != nil {
			log.Warn("interrupted; skipping remaining pairs", "remaining", len(pairs)-i)
			break
		}
		if rend != nil {
			rend.StartPair(fmt.Sprintf("[%d/%d] %s", i+1, len(pairs),
				pairLabel(pairs[i].Name, pairs[i].From.Repo, pairs[i].To.Repo)))
		}
		res := r.RunPair(ctx, &pairs[i])
		results = append(results, res)
		if !res.OK() {
			failed++
		}
		if rend != nil {
			detail := fmt.Sprintf("copied %d, skipped %d", res.Copied, res.Skipped)
			if !res.OK() {
				detail = res.Error
			}
			rend.FinishPair(res.OK(), detail, res.Duration)
		}
	}

	if !flagDryRun {
		sendNotification(log, cfg, "run", started, results)
	}

	ok := len(results) - failed
	if rend != nil {
		rend.Summary(failed == 0, fmt.Sprintf("%d succeeded, %d failed  (%s)",
			ok, failed, time.Since(started).Round(time.Second)))
	} else {
		summary := log.With("succeeded", ok, "failed", failed, "duration", time.Since(started).Round(time.Second))
		if failed > 0 {
			summary.Error("run finished with failures")
		} else if ctx.Err() == nil {
			summary.Info("run finished")
		}
	}
	if failed > 0 {
		os.Exit(2)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("run interrupted")
	}
	return nil
}

func selectPairs(cfg *config.Config, names []string) ([]config.Pair, error) {
	if len(names) == 0 {
		return cfg.Pairs, nil
	}
	var out []config.Pair
	for _, n := range names {
		i := slices.IndexFunc(cfg.Pairs, func(p config.Pair) bool { return p.Name == n })
		if i < 0 {
			return nil, fmt.Errorf("no pair named %q in config", n)
		}
		out = append(out, cfg.Pairs[i])
	}
	return out, nil
}
