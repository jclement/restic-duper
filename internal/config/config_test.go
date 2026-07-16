package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `
notifications:
  webhook:
    url: https://example.com/hook
    headers:
      Authorization: Bearer tok
pairs:
  - name: a-to-b
    from:
      repo: /repo/a
      password: secret
    to:
      repo: /repo/b
      password_file: /etc/pass
  - name: b-to-c
    from:
      repo: /repo/b
      password_command: pass show b
    to:
      repo: s3:example/bucket
      password: other
      env:
        AWS_ACCESS_KEY_ID: key
    snapshots: all
    copy_args: ["--host", "web1"]
    timeout: 2h
`

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Pairs) != 2 {
		t.Fatalf("want 2 pairs, got %d", len(cfg.Pairs))
	}
	if cfg.Pairs[0].Snapshots != "latest" {
		t.Errorf("default snapshots = %q, want latest", cfg.Pairs[0].Snapshots)
	}
	if cfg.Pairs[1].Snapshots != "all" {
		t.Errorf("snapshots = %q, want all", cfg.Pairs[1].Snapshots)
	}
	if cfg.Pairs[1].Timeout.Std() != 2*time.Hour {
		t.Errorf("timeout = %v, want 2h", cfg.Pairs[1].Timeout.Std())
	}
	w := cfg.Notifications.Webhook
	if w.Method != "POST" {
		t.Errorf("default method = %q, want POST", w.Method)
	}
	if !w.FireOnFailure() {
		t.Error("on_failure should default to true")
	}
	if w.OnSuccess {
		t.Error("on_success should default to false")
	}
	if w.Timeout.Std() != 30*time.Second {
		t.Errorf("default webhook timeout = %v, want 30s", w.Timeout.Std())
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"no pairs", `pairs: []`, "no pairs"},
		{"missing name", `
pairs:
  - from: {repo: /a, password: x}
    to: {repo: /b, password: x}`, "name is required"},
		{"duplicate name", `
pairs:
  - name: dup
    from: {repo: /a, password: x}
    to: {repo: /b, password: x}
  - name: dup
    from: {repo: /c, password: x}
    to: {repo: /d, password: x}`, "duplicate pair name"},
		{"no password", `
pairs:
  - name: p
    from: {repo: /a}
    to: {repo: /b, password: x}`, "one of password"},
		{"two passwords", `
pairs:
  - name: p
    from: {repo: /a, password: x, password_file: /f}
    to: {repo: /b, password: x}`, "mutually exclusive"},
		{"same repo", `
pairs:
  - name: p
    from: {repo: /a, password: x}
    to: {repo: /a, password: x}`, "same repository"},
		{"bad snapshots", `
pairs:
  - name: p
    from: {repo: /a, password: x}
    to: {repo: /b, password: x}
    snapshots: newest`, "latest"},
		{"env conflict", `
pairs:
  - name: p
    from:
      repo: s3:one
      password: x
      env: {AWS_ACCESS_KEY_ID: key1}
    to:
      repo: s3:two
      password: x
      env: {AWS_ACCESS_KEY_ID: key2}`, "different values"},
		{"webhook without url", `
notifications:
  webhook:
    method: POST
pairs:
  - name: p
    from: {repo: /a, password: x}
    to: {repo: /b, password: x}`, "url is required"},
		{"unknown field", `
pears:
  - nope`, "field pears not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("RD_TEST_SECRET", "s3cr3t")
	out, err := ExpandEnv([]byte("password: ${RD_TEST_SECRET} and $HOME and $5"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "s3cr3t") {
		t.Errorf("did not expand ${RD_TEST_SECRET}: %s", got)
	}
	if !strings.Contains(got, "$HOME") || !strings.Contains(got, "$5") {
		t.Errorf("bare $ tokens must be left alone: %s", got)
	}

	if _, err := ExpandEnv([]byte("x: ${RD_TEST_DEFINITELY_UNSET_VAR}")); err == nil {
		t.Error("expected error for unset variable")
	}
}

func TestMergedEnv(t *testing.T) {
	p := Pair{
		From: Repo{Env: map[string]string{"A": "1", "SHARED": "x"}},
		To:   Repo{Env: map[string]string{"B": "2", "SHARED": "x"}},
	}
	m := p.MergedEnv()
	if m["A"] != "1" || m["B"] != "2" || m["SHARED"] != "x" {
		t.Errorf("bad merge: %v", m)
	}
}
