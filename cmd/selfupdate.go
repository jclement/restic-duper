package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/jclement/restic-duper/internal/selfupdate"
)

var flagUpdateCheck bool

var selfUpdateCmd = &cobra.Command{
	Use:   "self-update",
	Short: "Replace this binary with the latest GitHub release",
	Long: `self-update downloads the latest release from
github.com/jclement/restic-duper, verifies it against the release's
checksums.txt, and atomically replaces the running executable.

If the binary lives in a root-owned directory (e.g. /usr/local/bin), run
with sudo.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		log := newLogger()
		tag, err := selfupdate.Update(cmd.Context(), log, version, flagUpdateCheck)
		if err != nil {
			return err
		}
		if flagUpdateCheck && tag != "" {
			fmt.Printf("update available: %s (current %s); run 'restic-duper self-update' to install\n", tag, version)
		}
		return nil
	},
}

func init() {
	selfUpdateCmd.Flags().BoolVar(&flagUpdateCheck, "check", false, "only check for a newer release, do not install")
	rootCmd.AddCommand(selfUpdateCmd)
}
