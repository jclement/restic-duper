package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jclement/restic-duper/internal/config"
	"github.com/jclement/restic-duper/internal/runner"
)

var flagConnect bool

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Validate the config file (and optionally repository connectivity)",
	Args:  cobra.NoArgs,
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
		log.Info("config is valid", "path", path, "pairs", len(cfg.Pairs))

		if !flagConnect {
			return nil
		}

		restic := cfg.ResticBinary
		if restic == "" {
			restic = "restic"
		}
		r := &runner.Runner{Restic: restic, Log: log, Verbose: flagVerbose}
		if err := r.CheckRestic(cmd.Context()); err != nil {
			return err
		}

		failures := 0
		for i := range cfg.Pairs {
			p := &cfg.Pairs[i]
			for _, side := range []struct {
				label string
				repo  *config.Repo
			}{{"from", &p.From}, {"to", &p.To}} {
				if err := probeRepo(restic, p, side.repo); err != nil {
					log.Error("repository unreachable", "pair", p.Name, "side", side.label, "repo", side.repo.Repo, "error", err)
					failures++
				} else {
					log.Info("repository ok", "pair", p.Name, "side", side.label, "repo", side.repo.Repo)
				}
			}
		}
		if failures > 0 {
			return fmt.Errorf("%d repositories unreachable", failures)
		}
		return nil
	},
}

// probeRepo checks a repository is reachable and the password works by
// reading its config object.
func probeRepo(restic string, p *config.Pair, r *config.Repo) error {
	cmd := exec.Command(restic, "--repo", r.Repo, "--no-lock", "cat", "config")
	env := os.Environ()
	for k, v := range p.MergedEnv() {
		env = append(env, k+"="+v)
	}
	env = append(env, runner.PasswordEnv("RESTIC", r)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, lastLine(out))
	}
	return nil
}

// lastLine returns the last non-empty line of command output.
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func init() {
	checkCmd.Flags().BoolVar(&flagConnect, "connect", false, "also verify each repository is reachable and unlockable")
	rootCmd.AddCommand(checkCmd)
}
