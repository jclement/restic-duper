package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jclement/restic-duper/internal/config"
	"github.com/jclement/restic-duper/internal/runner"
)

var flagStatusPairs []string

type sideStatus struct {
	Repo      string     `json:"repo"`
	Snapshots int        `json:"snapshots"`
	Latest    *time.Time `json:"latest,omitempty"`
	SizeBytes int64      `json:"size_bytes"`
}

type pairStatus struct {
	Name   string      `json:"name"`
	Source *sideStatus `json:"source,omitempty"`
	Dest   *sideStatus `json:"dest,omitempty"`
	InSync bool        `json:"in_sync"`
	Error  string      `json:"error,omitempty"`
}

var statusCmd = &cobra.Command{
	Use:     "status",
	Aliases: []string{"verify"},
	Short:   "Show snapshot counts, last backup times, sizes, and replication state per pair",
	Long: `status inspects both repositories of every pair (read-only, no locks) and
reports snapshot count, most recent snapshot time, and repository size.

It also verifies replication: the pair is "in sync" when the destination
contains a copy of the source's latest snapshot (matched via the "original"
field restic copy records). Exit code is 2 if any repository is unreachable
or any pair is behind, so it can be scheduled as a verification step.`,
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

		pairs, err := selectPairs(cfg, flagStatusPairs)
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

		problems := 0
		var report []pairStatus
		for i := range pairs {
			if ctx.Err() != nil {
				return fmt.Errorf("status interrupted")
			}
			p := &pairs[i]
			ps := pairStatus{Name: p.Name}

			srcSnaps, srcErr := r.ListSnapshots(ctx, p, &p.From)
			dstSnaps, dstErr := r.ListSnapshots(ctx, p, &p.To)
			if srcErr != nil {
				ps.Error = "source: " + srcErr.Error()
			} else if dstErr != nil {
				ps.Error = "dest: " + dstErr.Error()
			}
			if ps.Error != "" {
				problems++
				report = append(report, ps)
				continue
			}

			ps.Source = sideReport(ctx, r, p, &p.From, srcSnaps, log)
			ps.Dest = sideReport(ctx, r, p, &p.To, dstSnaps, log)
			ps.InSync = runner.InSync(srcSnaps, dstSnaps)
			if !ps.InSync {
				problems++
			}
			report = append(report, ps)
		}

		if flagJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(report); err != nil {
				return err
			}
		} else {
			printStatusTable(report)
		}
		if problems > 0 {
			log.Error("status found problems", "count", problems)
			os.Exit(2)
		}
		return nil
	},
}

func sideReport(ctx context.Context, r *runner.Runner, p *config.Pair, side *config.Repo, snaps []runner.Snapshot, log *slog.Logger) *sideStatus {
	s := &sideStatus{Repo: runner.RedactRepo(side.Repo), Snapshots: len(snaps)}
	if latest := runner.Latest(snaps); latest != nil {
		t := latest.Time
		s.Latest = &t
	}
	size, err := r.RawDataSize(ctx, p, side)
	if err != nil {
		log.Warn("could not read repository size", "pair", p.Name, "repo", s.Repo, "error", err)
		s.SizeBytes = -1
	} else {
		s.SizeBytes = size
	}
	return s
}

func printStatusTable(report []pairStatus) {
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PAIR\tSIDE\tREPO\tSNAPSHOTS\tLATEST\tSIZE\tSTATE")
	for _, ps := range report {
		if ps.Error != "" {
			fmt.Fprintf(w, "%s\t\t\t\t\t\tUNREACHABLE: %s\n", ps.Name, ps.Error)
			continue
		}
		state := "in sync"
		if !ps.InSync {
			state = "BEHIND"
		}
		fmt.Fprintf(w, "%s\tsource\t%s\t%d\t%s\t%s\t\n",
			ps.Name, ps.Source.Repo, ps.Source.Snapshots, formatLatest(ps.Source.Latest), humanBytes(ps.Source.SizeBytes))
		fmt.Fprintf(w, "\tdest\t%s\t%d\t%s\t%s\t%s\n",
			ps.Dest.Repo, ps.Dest.Snapshots, formatLatest(ps.Dest.Latest), humanBytes(ps.Dest.SizeBytes), state)
	}
	w.Flush()
}

func formatLatest(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return fmt.Sprintf("%s (%s ago)", t.Local().Format("2006-01-02 15:04"), humanAge(time.Since(*t)))
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func humanBytes(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func init() {
	statusCmd.Flags().StringSliceVar(&flagStatusPairs, "pair", nil, "only the named pair(s); repeatable")
	rootCmd.AddCommand(statusCmd)
}
