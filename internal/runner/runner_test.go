package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jclement/restic-duper/internal/config"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildArgs(t *testing.T) {
	p := &config.Pair{
		From:      config.Repo{Repo: "/src"},
		To:        config.Repo{Repo: "/dst"},
		Snapshots: "latest",
		CopyArgs:  []string{"--host", "web1"},
	}
	got := BuildArgs(p)
	want := []string{"copy", "--verbose", "--repo", "/dst", "--from-repo", "/src", "--host", "web1", "latest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildArgs = %v, want %v", got, want)
	}

	p.Snapshots = "all"
	p.CopyArgs = nil
	got = BuildArgs(p)
	want = []string{"copy", "--verbose", "--repo", "/dst", "--from-repo", "/src"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildArgs(all) = %v, want %v", got, want)
	}
}

func TestBuildEnv(t *testing.T) {
	p := &config.Pair{
		From: config.Repo{Repo: "/src", PasswordFile: "/src.pass", Env: map[string]string{"B2_ACCOUNT_ID": "abc"}},
		To:   config.Repo{Repo: "/dst", Password: "topsecret"},
	}
	env := BuildEnv(p)
	find := func(k string) (string, bool) {
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, k+"="); ok {
				return v, true
			}
		}
		return "", false
	}
	if v, _ := find("RESTIC_PASSWORD"); v != "topsecret" {
		t.Errorf("RESTIC_PASSWORD = %q", v)
	}
	if v, _ := find("RESTIC_FROM_PASSWORD_FILE"); v != "/src.pass" {
		t.Errorf("RESTIC_FROM_PASSWORD_FILE = %q", v)
	}
	if _, ok := find("RESTIC_FROM_PASSWORD"); ok {
		t.Error("RESTIC_FROM_PASSWORD must not be set when password_file is used")
	}
	if v, _ := find("B2_ACCOUNT_ID"); v != "abc" {
		t.Errorf("B2_ACCOUNT_ID = %q", v)
	}
	if _, ok := find("PATH"); !ok {
		t.Error("process environment must be inherited")
	}
}

func TestBuildEnvScrubsAmbientResticVars(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD_FILE", "/ambient/leak")
	t.Setenv("RESTIC_FROM_REPOSITORY", "/ambient/repo")
	t.Setenv("RESTIC_CACHE_DIR", "/keep/me")
	p := &config.Pair{
		From: config.Repo{Repo: "/src", Password: "a"},
		To:   config.Repo{Repo: "/dst", Password: "b"},
	}
	env := BuildEnv(p)
	for _, e := range env {
		if strings.HasPrefix(e, "RESTIC_PASSWORD_FILE=") || strings.HasPrefix(e, "RESTIC_FROM_REPOSITORY=") {
			t.Errorf("ambient credential variable leaked: %s", e)
		}
	}
	if !slicesContains(env, "RESTIC_CACHE_DIR=/keep/me") {
		t.Error("non-credential restic variables must pass through")
	}
}

func slicesContains(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func TestRedactRepo(t *testing.T) {
	in := "rest:https://user:hunter2@backup.example.com/repo"
	got := RedactRepo(in)
	if strings.Contains(got, "hunter2") {
		t.Errorf("password not redacted: %s", got)
	}
	if got != "rest:https://user:***@backup.example.com/repo" {
		t.Errorf("unexpected redaction: %s", got)
	}
	if plain := RedactRepo("/srv/restic/main"); plain != "/srv/restic/main" {
		t.Errorf("plain path mangled: %s", plain)
	}
}

func TestCopyCounter(t *testing.T) {
	c := &copyCounter{}
	for _, line := range []string{
		"snapshot 79766175 saved",
		"snapshot ae1b88f9 saved, copied from source snapshot aae4da24",
		"skipping snapshot 5b8d1a9c, already present in repo",
		"skipping source snapshot aae4da24, was already copied to snapshot ae1b88f9",
		"some other noise",
	} {
		c.observe(line)
	}
	copied, skipped := c.totals()
	if copied != 2 || skipped != 2 {
		t.Errorf("copied=%d skipped=%d, want 2/2", copied, skipped)
	}
	if !strings.Contains(c.lastLines(), "noise") {
		t.Errorf("lastLines missing recent output: %q", c.lastLines())
	}
}

func TestLineWriterSplitsAndFlushes(t *testing.T) {
	c := &copyCounter{}
	w := &lineWriter{log: discard(), stream: "stdout", counter: c}
	io.WriteString(w, "snapshot 79766175 saved\npartial")
	io.WriteString(w, " line\r\nsnapshot ff00ff00 sav")
	io.WriteString(w, "ed\ntrailing without newline")
	w.flush()
	copied, _ := c.totals()
	if copied != 2 {
		t.Errorf("copied = %d, want 2", copied)
	}
	if !strings.Contains(c.lastLines(), "trailing without newline") {
		t.Errorf("flush lost trailing data: %q", c.lastLines())
	}
}

func TestParseResticVersion(t *testing.T) {
	v, ok := parseResticVersion("restic 0.18.1 compiled with go1.24 on darwin/arm64")
	if !ok || v != [3]int{0, 18, 1} {
		t.Errorf("got %v ok=%v", v, ok)
	}
	if _, ok := parseResticVersion("garbage"); ok {
		t.Error("expected parse failure")
	}
	if !versionLess([3]int{0, 14, 9}, MinResticVersion) {
		t.Error("0.14.9 should be < 0.15.0")
	}
	if versionLess([3]int{0, 15, 0}, MinResticVersion) {
		t.Error("0.15.0 should not be < 0.15.0")
	}
}

// TestRunPairWithFakeRestic exercises RunPair end to end using a stub
// restic script that verifies its arguments and environment.
func TestRunPairWithFakeRestic(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "restic")
	script := `#!/bin/sh
if [ "$1" = "version" ]; then echo "restic 0.18.0 compiled"; exit 0; fi
[ "$RESTIC_PASSWORD" = "dstpass" ] || { echo "bad dst password" >&2; exit 1; }
[ "$RESTIC_FROM_PASSWORD" = "srcpass" ] || { echo "bad src password" >&2; exit 1; }
echo "snapshot cafe0001 saved"
echo "skipping snapshot beef0002, already present in repo"
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	r := &Runner{Restic: stub, Log: discard()}
	if err := r.CheckRestic(context.Background()); err != nil {
		t.Fatalf("CheckRestic: %v", err)
	}
	p := &config.Pair{
		Name:      "test",
		From:      config.Repo{Repo: "/src", Password: "srcpass"},
		To:        config.Repo{Repo: "/dst", Password: "dstpass"},
		Snapshots: "latest",
	}
	res := r.RunPair(context.Background(), p)
	if !res.OK() {
		t.Fatalf("RunPair failed: %s", res.Error)
	}
	if res.Copied != 1 || res.Skipped != 1 {
		t.Errorf("copied=%d skipped=%d, want 1/1", res.Copied, res.Skipped)
	}
}

// A zero-snapshot copy (restic exit 0 with "no snapshot matched") must fail
// the pair unless allow_empty is set — this is the tool's core false-success
// guard.
func TestRunPairNoMatchIsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "restic")
	script := `#!/bin/sh
echo 'Ignoring "latest": no snapshot matched given filter (Paths:[] Tags:[] Hosts:[nosuchhost])' >&2
exit 0
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Restic: stub, Log: discard()}
	p := &config.Pair{
		Name: "empty",
		From: config.Repo{Repo: "/src", Password: "x"},
		To:   config.Repo{Repo: "/dst", Password: "y"},
	}
	res := r.RunPair(context.Background(), p)
	if res.OK() {
		t.Fatal("zero matched snapshots must be a failure by default")
	}
	if !strings.Contains(res.Error, "no snapshot matched") {
		t.Errorf("error should carry restic's warning: %q", res.Error)
	}

	p.AllowEmpty = true
	if res := r.RunPair(context.Background(), p); !res.OK() {
		t.Errorf("allow_empty pair must succeed: %s", res.Error)
	}
}

// EnsureRepo must init only on restic's dedicated "repository does not
// exist" exit code (10), and must pass --copy-chunker-params --from-repo.
func TestEnsureRepoInitsOnlyOnExit10(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "restic")
	marker := filepath.Join(dir, "repo-exists")
	argsLog := filepath.Join(dir, "init-args")
	script := `#!/bin/sh
if [ "$1" = "init" ] || [ "$3" = "init" ]; then
  echo "$@" > ` + argsLog + `
  touch ` + marker + `
  exit 0
fi
[ -f ` + marker + ` ] && exit 0
echo "Fatal: repository does not exist" >&2
exit 10
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Restic: stub, Log: discard()}
	p := &config.Pair{
		Name: "boot",
		From: config.Repo{Repo: "/src", Password: "a"},
		To:   config.Repo{Repo: "/dst", Password: "b"},
	}
	inited, err := r.EnsureRepo(context.Background(), p)
	if err != nil || !inited {
		t.Fatalf("first EnsureRepo: inited=%v err=%v, want true/nil", inited, err)
	}
	args, _ := os.ReadFile(argsLog)
	for _, want := range []string{"--copy-chunker-params", "--from-repo /src", "--repo /dst"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("init args missing %q: %s", want, args)
		}
	}
	inited, err = r.EnsureRepo(context.Background(), p)
	if err != nil || inited {
		t.Fatalf("second EnsureRepo: inited=%v err=%v, want false/nil", inited, err)
	}
}

func TestEnsureRepoRefusesAmbiguousErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "restic")
	// Exit 12 = wrong password: must NOT trigger init.
	script := "#!/bin/sh\nif [ \"$1\" = init ] || [ \"$3\" = init ]; then echo INIT-RAN > " + filepath.Join(dir, "init-ran") + "; fi\necho 'Fatal: wrong password or no key found' >&2\nexit 12\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Restic: stub, Log: discard()}
	p := &config.Pair{
		Name: "locked",
		From: config.Repo{Repo: "/src", Password: "a"},
		To:   config.Repo{Repo: "/dst", Password: "wrong"},
	}
	inited, err := r.EnsureRepo(context.Background(), p)
	if err == nil || inited {
		t.Fatalf("wrong password must be an error, not an init: inited=%v err=%v", inited, err)
	}
	if !strings.Contains(err.Error(), "wrong password") {
		t.Errorf("error should carry restic output: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "init-ran")); statErr == nil {
		t.Error("init must not run on ambiguous errors")
	}
}

func TestLatestAndInSync(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	src := []Snapshot{
		{ID: "old", Time: t0},
		{ID: "new", Time: t0.Add(24 * time.Hour)},
	}
	if l := Latest(src); l == nil || l.ID != "new" {
		t.Fatalf("Latest = %+v", l)
	}
	if Latest(nil) != nil {
		t.Error("Latest(nil) must be nil")
	}

	dstSynced := []Snapshot{{ID: "copy1", Original: "old"}, {ID: "copy2", Original: "new"}}
	dstBehind := []Snapshot{{ID: "copy1", Original: "old"}}
	if !InSync(src, dstSynced) {
		t.Error("dest with copy of latest must be in sync")
	}
	if InSync(src, dstBehind) {
		t.Error("dest missing latest must be behind")
	}
	if !InSync(nil, nil) {
		t.Error("empty source is trivially in sync")
	}
}

// ForgetPair must target the destination only, carry the keep policy, and
// respect prune/dry-run flags.
func TestForgetPair(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "restic")
	argsLog := filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > " + argsLog + "\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Restic: stub, Log: discard()}
	p := &config.Pair{
		Name:      "ret",
		From:      config.Repo{Repo: "/src", Password: "a"},
		To:        config.Repo{Repo: "/dst", Password: "b"},
		Retention: &config.Retention{KeepDaily: 7, ForgetArgs: []string{"--group-by", "host"}},
	}
	if res := r.ForgetPair(context.Background(), p, true, false); !res.OK() {
		t.Fatalf("ForgetPair: %s", res.Error)
	}
	args, _ := os.ReadFile(argsLog)
	got := strings.TrimSpace(string(args))
	for _, want := range []string{"forget --repo /dst", "--keep-daily 7", "--group-by host", "--prune"} {
		if !strings.Contains(got, want) {
			t.Errorf("args missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "/src") {
		t.Errorf("forget must never touch the source repo: %s", got)
	}

	if res := r.ForgetPair(context.Background(), p, false, true); !res.OK() {
		t.Fatalf("ForgetPair dry-run: %s", res.Error)
	}
	args, _ = os.ReadFile(argsLog)
	got = strings.TrimSpace(string(args))
	if !strings.Contains(got, "--dry-run") || strings.Contains(got, "--prune") {
		t.Errorf("dry-run without prune expected: %s", got)
	}
}

func TestUnmarshalResticJSONSkipsProgressLines(t *testing.T) {
	out := []byte("[0:00] 100.00%  1 / 1 snapshots, 10 blobs, 2.840 KiB\n{\"total_size\":2908}\n")
	var st struct {
		TotalSize int64 `json:"total_size"`
	}
	if err := unmarshalResticJSON(out, &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if st.TotalSize != 2908 {
		t.Errorf("total_size = %d", st.TotalSize)
	}
	if err := unmarshalResticJSON([]byte("garbage\nmore garbage"), &st); err == nil {
		t.Error("expected error for non-JSON output")
	}
}

func TestSupportsExitCodes(t *testing.T) {
	r := &Runner{}
	if r.SupportsExitCodes() {
		t.Error("unknown version must not claim exit-code support")
	}
	r.ver, r.verKnown = [3]int{0, 16, 5}, true
	if r.SupportsExitCodes() {
		t.Error("0.16 must not claim exit-code support")
	}
	r.ver = [3]int{0, 17, 0}
	if !r.SupportsExitCodes() {
		t.Error("0.17 must claim exit-code support")
	}
}

func TestRunPairFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "restic")
	script := "#!/bin/sh\necho \"Fatal: wrong password\" >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Restic: stub, Log: discard()}
	p := &config.Pair{
		Name: "bad",
		From: config.Repo{Repo: "/src", Password: "x"},
		To:   config.Repo{Repo: "/dst", Password: "y"},
	}
	res := r.RunPair(context.Background(), p)
	if res.OK() {
		t.Fatal("expected failure")
	}
	if !strings.Contains(res.Error, "wrong password") {
		t.Errorf("error should include restic output, got %q", res.Error)
	}
}
