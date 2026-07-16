// Package runner executes restic copy for each configured pair.
package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jclement/restic-duper/internal/config"
)

// MinResticVersion is the oldest restic release supporting the
// RESTIC_FROM_PASSWORD* environment variables used for the source repository.
var MinResticVersion = [3]int{0, 15, 0}

// Result is the outcome of one pair.
type Result struct {
	Name       string        `json:"name"`
	FromRepo   string        `json:"from_repo,omitempty"` // redacted
	ToRepo     string        `json:"to_repo,omitempty"`   // redacted
	Status     string        `json:"status"`              // "success" | "failure"
	Error      string        `json:"error,omitempty"`
	Duration   time.Duration `json:"-"`
	FinishedAt time.Time     `json:"-"`
	Seconds    float64       `json:"duration_seconds"`
	Copied     int           `json:"snapshots_copied"`
	Skipped    int           `json:"snapshots_skipped"` // already present in destination
}

func (r Result) OK() bool { return r.Status == "success" }

// ProgressEvent reports live progress parsed from restic's output.
type ProgressEvent struct {
	// SnapshotID is set (alone) when restic starts copying a new snapshot.
	SnapshotID string
	// Percent/Done/Total describe a progress tick, e.g. "54.2% 122/226 packs".
	Percent     float64
	Done, Total int
	Unit        string // "packs", "blobs", ...
}

// Runner drives restic.
type Runner struct {
	Restic string // path to restic binary
	Log    *slog.Logger
	DryRun bool
	// Verbose streams every line of restic output at INFO instead of DEBUG.
	Verbose bool
	// Progress, when set, receives live events parsed from restic output.
	// Setting it also asks restic to emit progress lines to the pipe
	// (RESTIC_PROGRESS_FPS).
	Progress func(ProgressEvent)

	ver      [3]int // detected restic version, set by CheckRestic
	verKnown bool
}

// exitRepoDoesNotExist is restic's dedicated exit code (>= 0.17) for
// "repository does not exist", distinct from wrong password (12) and
// generic failures (1).
const exitRepoDoesNotExist = 10

// SupportsExitCodes reports whether the detected restic emits the specific
// per-failure exit codes introduced in 0.17.
func (r *Runner) SupportsExitCodes() bool {
	return r.verKnown && !versionLess(r.ver, [3]int{0, 17, 0})
}

// CheckRestic verifies the restic binary exists and is recent enough.
func (r *Runner) CheckRestic(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, r.Restic, "version").Output()
	if err != nil {
		return fmt.Errorf("cannot run %q: %w (is restic installed and on PATH?)", r.Restic, err)
	}
	ver, ok := parseResticVersion(string(out))
	if !ok {
		r.Log.Warn("could not parse restic version; continuing", "output", strings.TrimSpace(string(out)))
		return nil
	}
	r.ver, r.verKnown = ver, true
	r.Log.Debug("restic detected", "version", fmt.Sprintf("%d.%d.%d", ver[0], ver[1], ver[2]))
	if versionLess(ver, MinResticVersion) {
		return fmt.Errorf("restic %d.%d.%d is too old: restic-duper needs >= %d.%d.%d for RESTIC_FROM_* support",
			ver[0], ver[1], ver[2], MinResticVersion[0], MinResticVersion[1], MinResticVersion[2])
	}
	return nil
}

var versionRe = regexp.MustCompile(`restic (\d+)\.(\d+)\.(\d+)`)

func parseResticVersion(s string) ([3]int, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := 0; i < 3; i++ {
		fmt.Sscanf(m[i+1], "%d", &v[i])
	}
	return v, true
}

func versionLess(a, b [3]int) bool {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// BuildArgs constructs the restic copy argument list for a pair.
// --verbose is always passed: without it restic (>= 0.16) is silent about
// snapshots it skips, and we count those for reporting.
func BuildArgs(p *config.Pair) []string {
	args := []string{"copy", "--verbose", "--repo", p.To.Repo, "--from-repo", p.From.Repo}
	args = append(args, p.CopyArgs...)
	if p.Snapshots == "latest" {
		args = append(args, "latest")
	}
	// "all": no snapshot IDs -> restic copies every snapshot matching filters.
	return args
}

// scrubVars are restic variables that select repositories or credentials.
// Ambient values inherited from the parent shell must never leak into the
// child process: a stray RESTIC_PASSWORD_FILE would silently override a
// pair's configured password (restic prefers _FILE/_COMMAND over _PASSWORD).
var scrubVars = map[string]bool{
	"RESTIC_REPOSITORY":            true,
	"RESTIC_REPOSITORY_FILE":       true,
	"RESTIC_PASSWORD":              true,
	"RESTIC_PASSWORD_FILE":         true,
	"RESTIC_PASSWORD_COMMAND":      true,
	"RESTIC_KEY_HINT":              true,
	"RESTIC_FROM_REPOSITORY":       true,
	"RESTIC_FROM_REPOSITORY_FILE":  true,
	"RESTIC_FROM_PASSWORD":         true,
	"RESTIC_FROM_PASSWORD_FILE":    true,
	"RESTIC_FROM_PASSWORD_COMMAND": true,
	"RESTIC_FROM_KEY_HINT":         true,
}

// ScrubEnv returns env without any repository/credential-selecting restic
// variables. Cache, compression, and backend variables pass through.
func ScrubEnv(env []string) []string {
	out := env[:0:0]
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		if !scrubVars[name] {
			out = append(out, e)
		}
	}
	return out
}

// BuildEnv constructs the child process environment for a pair: the current
// process environment (scrubbed of ambient restic credential variables),
// the pair's backend credentials, then the password variables for both
// sides. Passwords never appear on the command line.
func BuildEnv(p *config.Pair) []string {
	env := ScrubEnv(os.Environ())
	for k, v := range p.MergedEnv() {
		env = append(env, k+"="+v)
	}
	env = append(env, PasswordEnv("RESTIC", &p.To)...)
	env = append(env, PasswordEnv("RESTIC_FROM", &p.From)...)
	return env
}

// repoSecretRe matches userinfo passwords embedded in repository URLs,
// e.g. rest:https://user:secret@host/repo.
var repoSecretRe = regexp.MustCompile(`(://[^/@:\s]+):[^@/\s]+@`)

// RedactRepo masks embedded credentials in a repository spec for logging.
func RedactRepo(s string) string {
	return repoSecretRe.ReplaceAllString(s, "$1:***@")
}

func PasswordEnv(prefix string, r *config.Repo) []string {
	switch {
	case r.Password != "":
		return []string{prefix + "_PASSWORD=" + r.Password}
	case r.PasswordFile != "":
		return []string{prefix + "_PASSWORD_FILE=" + r.PasswordFile}
	case r.PasswordCommand != "":
		return []string{prefix + "_PASSWORD_COMMAND=" + r.PasswordCommand}
	}
	return nil
}

// SideEnv builds the environment for running restic against a single side
// of a pair (RESTIC_PASSWORD* for that side only), e.g. for probes and
// status queries.
func SideEnv(p *config.Pair, side *config.Repo) []string {
	env := ScrubEnv(os.Environ())
	for k, v := range p.MergedEnv() {
		env = append(env, k+"="+v)
	}
	return append(env, PasswordEnv("RESTIC", side)...)
}

// Snapshot is the subset of restic's snapshot JSON we need.
type Snapshot struct {
	ID       string    `json:"id"`
	Original string    `json:"original"` // source snapshot ID when created by restic copy
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
}

// ListSnapshots returns all snapshots in one side's repository.
func (r *Runner) ListSnapshots(ctx context.Context, p *config.Pair, side *config.Repo) ([]Snapshot, error) {
	out, err := r.sideJSON(ctx, p, side, "snapshots")
	if err != nil {
		return nil, err
	}
	var snaps []Snapshot
	if err := unmarshalResticJSON(out, &snaps); err != nil {
		return nil, fmt.Errorf("parsing restic snapshots output: %w", err)
	}
	return snaps, nil
}

// RawDataSize returns the repository's packed data size in bytes.
func (r *Runner) RawDataSize(ctx context.Context, p *config.Pair, side *config.Repo) (int64, error) {
	out, err := r.sideJSON(ctx, p, side, "stats", "--mode", "raw-data")
	if err != nil {
		return 0, err
	}
	var st struct {
		TotalSize int64 `json:"total_size"`
	}
	if err := unmarshalResticJSON(out, &st); err != nil {
		return 0, fmt.Errorf("parsing restic stats output: %w", err)
	}
	return st.TotalSize, nil
}

// unmarshalResticJSON tolerates restic's habit of writing progress lines to
// stdout ahead of the JSON document (observed with stats, even under
// --quiet): it tries the whole output first, then each line from the last
// upward.
func unmarshalResticJSON(out []byte, v any) error {
	firstErr := json.Unmarshal(out, v)
	if firstErr == nil {
		return nil
	}
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || (line[0] != '{' && line[0] != '[') {
			continue
		}
		if err := json.Unmarshal(line, v); err == nil {
			return nil
		}
	}
	return firstErr
}

func (r *Runner) sideJSON(ctx context.Context, p *config.Pair, side *config.Repo, args ...string) ([]byte, error) {
	// --quiet: restic writes progress lines to stdout even when not on a
	// terminal (e.g. stats), which would corrupt the JSON document.
	full := append([]string{"--repo", side.Repo, "--no-lock", "--json", "--quiet"}, args...)
	cmd := exec.CommandContext(ctx, r.Restic, full...)
	cmd.Env = SideEnv(p, side)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("restic %s on %s: %w: %s",
			args[0], RedactRepo(side.Repo), err, RedactRepo(lastNonEmptyLine(stderr.Bytes())))
	}
	return out, nil
}

// Latest returns the most recent snapshot, or nil for an empty list.
func Latest(snaps []Snapshot) *Snapshot {
	var latest *Snapshot
	for i := range snaps {
		if latest == nil || snaps[i].Time.After(latest.Time) {
			latest = &snaps[i]
		}
	}
	return latest
}

// InSync reports whether the destination contains the source's latest
// snapshot — either as a direct copy (destination snapshot's "original"
// points at it) or, unusually, under the same ID.
func InSync(src, dst []Snapshot) bool {
	latest := Latest(src)
	if latest == nil {
		return true // nothing to replicate
	}
	for _, d := range dst {
		if d.ID == latest.ID || d.Original == latest.ID {
			return true
		}
	}
	return false
}

// EnsureRepo initializes the destination repository of a pair if — and only
// if — restic reports it does not exist (exit code 10). Any other failure
// (wrong password, network error, bad path) is returned as-is: creating a
// repository on an ambiguous error could silently fork the offsite backups
// to an unintended location. Returns true if the repository was created.
func (r *Runner) EnsureRepo(ctx context.Context, p *config.Pair) (bool, error) {
	log := r.Log.With("pair", p.Name)

	probe := exec.CommandContext(ctx, r.Restic, "--repo", p.To.Repo, "--no-lock", "cat", "config")
	probe.Env = BuildEnv(p)
	out, err := probe.CombinedOutput()
	if err == nil {
		log.Debug("destination repository exists", "repo", RedactRepo(p.To.Repo))
		return false, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != exitRepoDoesNotExist {
		return false, fmt.Errorf("cannot open destination %s (not initializing: only \"repository does not exist\" triggers init): %w: %s",
			RedactRepo(p.To.Repo), err, RedactRepo(lastNonEmptyLine(out)))
	}

	log.Warn("destination repository does not exist; initializing",
		"repo", RedactRepo(p.To.Repo), "chunker_params_from", RedactRepo(p.From.Repo))
	init := exec.CommandContext(ctx, r.Restic, "init",
		"--repo", p.To.Repo, "--from-repo", p.From.Repo, "--copy-chunker-params")
	init.Env = BuildEnv(p)
	if out, err := init.CombinedOutput(); err != nil {
		return false, fmt.Errorf("restic init of %s failed: %w: %s",
			RedactRepo(p.To.Repo), err, RedactRepo(lastNonEmptyLine(out)))
	}
	log.Warn("destination repository initialized", "repo", RedactRepo(p.To.Repo))
	return true, nil
}

func lastNonEmptyLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// RunPair executes restic copy for one pair.
func (r *Runner) RunPair(ctx context.Context, p *config.Pair) Result {
	log := r.Log.With("pair", p.Name)
	args := BuildArgs(p)
	res := Result{Name: p.Name, Status: "success",
		FromRepo: RedactRepo(p.From.Repo), ToRepo: RedactRepo(p.To.Repo)}

	if r.DryRun {
		log.Info("dry-run: would execute", "cmd", r.Restic+" "+strings.Join(args, " "))
		return res
	}

	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout.Std())
		defer cancel()
	}

	log.Info("copy started", "from", RedactRepo(p.From.Repo), "to", RedactRepo(p.To.Repo), "snapshots", p.Snapshots)
	start := time.Now()

	cmd := exec.CommandContext(ctx, r.Restic, args...)
	cmd.Env = BuildEnv(p)
	if r.Progress != nil {
		// Ask restic to emit progress lines to the (non-tty) pipe.
		cmd.Env = append(cmd.Env, "RESTIC_PROGRESS_FPS=10")
	}
	// On cancellation (Ctrl-C, SIGTERM, pair timeout) ask restic to stop
	// gracefully so it can release its repository locks; escalate to SIGKILL
	// only if it has not exited within WaitDelay. Go's default would SIGKILL
	// immediately, leaving stale locks in both repositories.
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	}
	cmd.WaitDelay = 30 * time.Second
	counter := &copyCounter{}
	stdout := &lineWriter{log: log, stream: "stdout", counter: counter, verbose: r.Verbose, progress: r.Progress}
	stderr := &lineWriter{log: log, stream: "stderr", counter: counter, verbose: r.Verbose, progress: r.Progress}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	stdout.flush()
	stderr.flush()
	res.Duration = time.Since(start)
	res.FinishedAt = time.Now().UTC()
	res.Seconds = res.Duration.Round(time.Millisecond).Seconds()
	res.Copied, res.Skipped = counter.totals()

	if err != nil {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			err = fmt.Errorf("timed out after %s", p.Timeout.Std())
		case context.Canceled:
			err = fmt.Errorf("interrupted by signal (%v)", err)
		}
		res.Status = "failure"
		res.Error = err.Error()
		if lines := counter.lastLines(); lines != "" {
			res.Error += ": " + lines
		}
		res.Error = RedactRepo(res.Error)
		log.Error("copy failed", "error", res.Error, "duration", res.Duration.Round(time.Second))
		return res
	}

	// restic copy exits 0 even when its snapshot filter matched nothing
	// ("Ignoring \"latest\": no snapshot matched given filter"). For a
	// replication tool that is a failure, not a success: a healthy run
	// always saves or skips at least one snapshot.
	if res.Copied+res.Skipped == 0 && !p.AllowEmpty {
		res.Status = "failure"
		res.Error = "restic copy matched no snapshots — source repository is empty or copy_args filters matched nothing " +
			"(set allow_empty: true for this pair if that is expected)"
		if line := counter.ignoredLine(); line != "" {
			res.Error += ": " + line
		}
		log.Error("copy failed", "error", res.Error, "duration", res.Duration.Round(time.Second))
		return res
	}

	log.Info("copy finished",
		"copied", res.Copied, "skipped", res.Skipped,
		"duration", res.Duration.Round(time.Second))
	return res
}

// ForgetPair applies the pair's retention policy to the DESTINATION
// repository via restic forget (optionally with --prune). The source
// repository is never touched.
func (r *Runner) ForgetPair(ctx context.Context, p *config.Pair, prune, dryRun bool) Result {
	log := r.Log.With("pair", p.Name)
	res := Result{Name: p.Name, Status: "success",
		FromRepo: RedactRepo(p.From.Repo), ToRepo: RedactRepo(p.To.Repo)}

	args := []string{"forget", "--repo", p.To.Repo}
	args = append(args, p.Retention.Args()...)
	args = append(args, p.Retention.ForgetArgs...)
	if dryRun {
		args = append(args, "--dry-run")
	}
	if prune {
		args = append(args, "--prune")
	}

	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout.Std())
		defer cancel()
	}

	log.Info("forget started", "repo", RedactRepo(p.To.Repo), "policy", strings.Join(p.Retention.Args(), " "), "prune", prune, "dry_run", dryRun)
	start := time.Now()

	cmd := exec.CommandContext(ctx, r.Restic, args...)
	cmd.Env = SideEnv(p, &p.To)
	if runtime.GOOS != "windows" {
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	}
	cmd.WaitDelay = 30 * time.Second
	if r.Progress != nil {
		cmd.Env = append(cmd.Env, "RESTIC_PROGRESS_FPS=10")
	}
	counter := &copyCounter{}
	stdout := &lineWriter{log: log, stream: "stdout", counter: counter, verbose: r.Verbose, progress: r.Progress}
	stderr := &lineWriter{log: log, stream: "stderr", counter: counter, verbose: r.Verbose, progress: r.Progress}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	stdout.flush()
	stderr.flush()
	res.Duration = time.Since(start)
	res.FinishedAt = time.Now().UTC()
	res.Seconds = res.Duration.Round(time.Millisecond).Seconds()

	if err != nil {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			err = fmt.Errorf("timed out after %s", p.Timeout.Std())
		case context.Canceled:
			err = fmt.Errorf("interrupted by signal (%v)", err)
		}
		res.Status = "failure"
		res.Error = err.Error()
		if lines := counter.lastLines(); lines != "" {
			res.Error += ": " + lines
		}
		res.Error = RedactRepo(res.Error)
		log.Error("forget failed", "error", res.Error, "duration", res.Duration.Round(time.Second))
		return res
	}

	log.Info("forget finished", "duration", res.Duration.Round(time.Second))
	return res
}

// copyCounter tallies restic copy progress lines and keeps recent output
// context for error reporting. It is written from the stdout and stderr
// copy goroutines concurrently, hence the mutex; cmd.Wait (inside Run)
// guarantees all writes complete before RunPair reads the totals.
type copyCounter struct {
	mu      sync.Mutex
	copied  int
	skipped int
	ignored string // restic's "no snapshot matched" warning, if seen
	recent  []string
}

// Restic's copy output has changed phrasing across versions:
//
//	snapshot 79766175 saved                                        (older)
//	snapshot ae1b88f9 saved, copied from source snapshot aae4da24  (0.16+)
//	skipping snapshot 5b8d1a9c, already present in repo            (older)
//	skipping source snapshot aae4da24, was already copied ...      (0.16+)
var (
	savedRe   = regexp.MustCompile(`^snapshot [0-9a-f]+ saved`)
	skippedRe = regexp.MustCompile(`^skipping (source )?snapshot [0-9a-f]+`)
	// restic warns and exits 0 when the snapshot filter matched nothing:
	//	Ignoring "latest": no snapshot matched given filter (Paths:[] Tags:[] Hosts:[])
	ignoredRe = regexp.MustCompile(`no snapshot matched`)
	// Progress lines restic emits when RESTIC_PROGRESS_FPS is set:
	//	[0:12] 54.24%  122 / 226 packs copied
	progressRe = regexp.MustCompile(`^\[[0-9:]+\] +([0-9.]+)% +([0-9]+) / ([0-9]+) ([a-z]+)`)
	// Header announcing which snapshot is being copied:
	//	snapshot d1ac293c of [/some/path] at 2026-07-16 ...
	snapStartRe = regexp.MustCompile(`^snapshot ([0-9a-f]+) of \[`)
)

func (c *copyCounter) observe(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case savedRe.MatchString(line):
		c.copied++
	case skippedRe.MatchString(line):
		c.skipped++
	case ignoredRe.MatchString(line):
		c.ignored = line
	}
	c.recent = append(c.recent, line)
	if len(c.recent) > 5 {
		c.recent = c.recent[1:]
	}
}

func (c *copyCounter) lastLines() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.recent, " | ")
}

func (c *copyCounter) totals() (copied, skipped int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.copied, c.skipped
}

func (c *copyCounter) ignoredLine() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ignored
}

// lineWriter splits process output into lines and logs each one. Write is
// called from exec's copy goroutine; splitting happens synchronously so no
// data is in flight once cmd.Run returns.
type lineWriter struct {
	log      *slog.Logger
	stream   string
	counter  *copyCounter
	verbose  bool
	progress func(ProgressEvent)
	buf      []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// flush logs any trailing output not terminated by a newline.
func (w *lineWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}

func (w *lineWriter) emit(line string) {
	line = strings.TrimRight(line, "\r")
	// restic redraws progress with carriage returns; keep only the final state.
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	w.counter.observe(line)
	if w.progress != nil {
		if m := progressRe.FindStringSubmatch(line); m != nil {
			var ev ProgressEvent
			fmt.Sscanf(m[1], "%f", &ev.Percent)
			fmt.Sscanf(m[2], "%d", &ev.Done)
			fmt.Sscanf(m[3], "%d", &ev.Total)
			ev.Unit = m[4]
			w.progress(ev)
			w.log.Debug(line, "stream", w.stream)
			return // progress ticks never go to INFO, even with -v
		}
		if m := snapStartRe.FindStringSubmatch(line); m != nil {
			w.progress(ProgressEvent{SnapshotID: m[1]})
		}
	}
	if w.verbose {
		w.log.Info(line, "stream", w.stream)
	} else {
		w.log.Debug(line, "stream", w.stream)
	}
}
