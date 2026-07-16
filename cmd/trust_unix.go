//go:build !windows

package cmd

import (
	"fmt"
	"os"
	"syscall"
)

// trustedConfig rejects auto-discovered config files that another user could
// have planted or modified: password_command executes arbitrary commands, so
// picking up a stranger's restic-duper.yaml from a shared working directory
// must not happen implicitly.
func trustedConfig(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Getuid(); int(st.Uid) != uid && st.Uid != 0 {
		return fmt.Errorf("not owned by you or root (owner uid %d)", st.Uid)
	}
	if info.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("world-writable (%s)", info.Mode().Perm())
	}
	return nil
}
