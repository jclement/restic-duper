package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/jclement/restic-duper/internal/config"
	"github.com/jclement/restic-duper/internal/runner"
)

var flagBootstrapPairs []string

var bootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Initialize destination repositories that do not exist yet",
	Long: `bootstrap probes the destination repository of every pair and runs
"restic init --copy-chunker-params --from-repo <source>" for the ones that do
not exist yet, so copied snapshots deduplicate against future direct backups.

It only creates a repository when restic reports the specific "repository
does not exist" condition (exit code 10, restic >= 0.17). Any other failure —
wrong password, network error, bad path — is reported and nothing is created,
so a typo cannot silently fork your offsite backups to a new location.

Run this once when setting up new pairs; "run" itself never creates
repositories.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		log := newLogger()
		path, err := configPath()
		if err != nil {
			return err
		}
		cfg, err := config.Load(path)
		if err != nil {
			return err
		}
		warnConfigPerms(log, path)

		pairs, err := selectPairs(cfg, flagBootstrapPairs)
		if err != nil {
			return err
		}

		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		r := &runner.Runner{Restic: cfg.ResticBinary, Log: log, Verbose: flagVerbose}
		if r.Restic == "" {
			r.Restic = "restic"
		}
		if err := r.CheckRestic(ctx); err != nil {
			return err
		}
		if !r.SupportsExitCodes() {
			return fmt.Errorf("bootstrap requires restic >= 0.17: it relies on restic's dedicated " +
				"\"repository does not exist\" exit code to avoid creating repositories on ambiguous errors")
		}

		created, existing, failed := 0, 0, 0
		for i := range pairs {
			if ctx.Err() != nil {
				log.Warn("interrupted; skipping remaining pairs", "remaining", len(pairs)-i)
				break
			}
			inited, err := r.EnsureRepo(ctx, &pairs[i])
			switch {
			case err != nil:
				log.Error("bootstrap failed", "pair", pairs[i].Name, "error", err)
				failed++
			case inited:
				created++
			default:
				log.Info("destination already initialized", "pair", pairs[i].Name, "repo", runner.RedactRepo(pairs[i].To.Repo))
				existing++
			}
		}

		summary := log.With("created", created, "existing", existing, "failed", failed)
		if failed > 0 {
			summary.Error("bootstrap finished with failures")
			os.Exit(2)
		}
		summary.Info("bootstrap finished")
		return nil
	},
}

func init() {
	bootstrapCmd.Flags().StringSliceVar(&flagBootstrapPairs, "pair", nil, "only bootstrap the named pair(s); repeatable")
	rootCmd.AddCommand(bootstrapCmd)
}
