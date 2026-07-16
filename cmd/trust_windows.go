//go:build windows

package cmd

// trustedConfig is a no-op on Windows, where POSIX ownership semantics do
// not apply.
func trustedConfig(string) error { return nil }
