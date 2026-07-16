// Package cmd implements the restic-duper CLI.
package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is injected at release time via -ldflags; for go-install builds
// it falls back to the module version from build info.
var version = "dev"

func init() {
	if version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
	}
	rootCmd.Version = version
}

var (
	flagConfig  string
	flagJSON    bool
	flagQuiet   bool
	flagVerbose bool
)

var rootCmd = &cobra.Command{
	Use:   "restic-duper",
	Short: "Replicate restic repositories with restic copy",
	Long: `restic-duper walks a list of configured repository pairs and uses
"restic copy" to replicate snapshots from each source repository into its
destination — a simple way to maintain offsite or redundant backups.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	Version:       version,
}

// Execute runs the CLI.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&flagConfig, "config", "c", "", "path to config file (default: ./restic-duper.yaml, ~/.config/restic-duper/config.yaml, /etc/restic-duper/config.yaml)")
	rootCmd.PersistentFlags().BoolVar(&flagJSON, "json", false, "log in JSON format")
	rootCmd.PersistentFlags().BoolVarP(&flagQuiet, "quiet", "q", false, "only log warnings and errors")
	rootCmd.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "stream full restic output")
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if flagQuiet {
		level = slog.LevelWarn
	}
	if flagVerbose {
		level = slog.LevelDebug
	}
	var h slog.Handler
	if flagJSON {
		h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	} else {
		h = newConsoleHandler(os.Stderr, level)
	}
	return slog.New(h)
}

// configPath resolves the config file location.
func configPath() (string, error) {
	if flagConfig != "" {
		return flagConfig, nil
	}
	candidates := []string{"restic-duper.yaml", "restic-duper.yml"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "restic-duper", "config.yaml"))
	}
	candidates = append(candidates, "/etc/restic-duper/config.yaml")
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			if err := trustedConfig(c); err != nil {
				return "", fmt.Errorf("refusing auto-discovered config %s: %v (pass it explicitly with --config to override)", c, err)
			}
			return c, nil
		}
	}
	return "", fmt.Errorf("no config file found (searched %v); use --config or run 'restic-duper init'", candidates)
}


// warnConfigPerms nags when a config file that may hold passwords is
// readable by other users.
func warnConfigPerms(log *slog.Logger, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o044 != 0 {
		log.Warn("config file is readable by other users; consider chmod 600", "path", path, "mode", info.Mode().Perm().String())
	}
}
