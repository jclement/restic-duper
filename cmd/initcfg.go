package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const exampleConfig = `# restic-duper configuration
# Copies snapshots between restic repositories using "restic copy".
# Any restic backend works on either side (local, sftp, s3, b2, rest, rclone, ...).
#
# ${VAR} references are expanded from the environment at load time.

# Optional: path to the restic binary (default: "restic" from PATH)
# restic_binary: /usr/local/bin/restic

notifications:
  webhook:
    url: https://example.com/hooks/restic-duper
    # method: POST            # default
    # headers:
    #   Authorization: Bearer ${WEBHOOK_TOKEN}
    on_failure: true          # default
    on_success: false         # default
    # timeout: 30s

pairs:
  - name: main-to-offsite
    from:
      repo: /srv/restic/main
      password_file: /etc/restic/main.pass
    to:
      repo: s3:s3.us-east-1.amazonaws.com/my-offsite-bucket/restic
      password: ${OFFSITE_RESTIC_PASSWORD}
      env:
        AWS_ACCESS_KEY_ID: ${OFFSITE_AWS_KEY}
        AWS_SECRET_ACCESS_KEY: ${OFFSITE_AWS_SECRET}
    # snapshots: latest       # "latest" (default) or "all"
    # copy_args: ["--host", "myserver"]   # extra args passed to restic copy
    # timeout: 6h
`

var initCmd = &cobra.Command{
	Use:   "init [path]",
	Short: "Write an example config file",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		path := "restic-duper.yaml"
		if len(args) == 1 {
			path = args[0]
		}
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; refusing to overwrite", path)
		}
		if err := os.WriteFile(path, []byte(exampleConfig), 0o600); err != nil {
			return err
		}
		fmt.Printf("wrote example config to %s\n", path)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}
