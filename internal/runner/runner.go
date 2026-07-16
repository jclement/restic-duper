// Package runner executes restic copy for each configured pair.
package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
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
	Name     string        `json:"name"`
	Status   string        `json:"status"` // "success" | "failure"
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"-"`
	Seconds  float64       `json:"duration_seconds"`
	Copied   int           `json:"snapshots_copied"`
	Skipped  int           `json:"snapshots_skipped"` // already present in destination
}

func (r Result) OK() bool { return r.Status == "success" }

// Runner drives restic.
type Runner struct {
	Restic string // path to restic binary
	Log    *slog.Logger
	DryRun bool
	// Verbose streams every line of restic output at INFO instead of DEBUG.
	Verbose bool
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

// BuildEnv constructs the child process environment for a pair: the current
// process environment, the pair's backend credentials, then the password
// variables for both sides. Passwords never appear on the command line.
func BuildEnv(p *config.Pair) []string {
	env := os.Environ()
	for k, v := range p.MergedEnv() {
		env = append(env, k+"="+v)
	}
	env = append(env, PasswordEnv("RESTIC", &p.To)...)
	env = append(env, PasswordEnv("RESTIC_FROM", &p.From)...)
	return env
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

// RunPair executes restic copy for one pair.
func (r *Runner) RunPair(ctx context.Context, p *config.Pair) Result {
	log := r.Log.With("pair", p.Name)
	args := BuildArgs(p)
	res := Result{Name: p.Name, Status: "success"}

	if r.DryRun {
		log.Info("dry-run: would execute", "cmd", r.Restic+" "+strings.Join(args, " "))
		return res
	}

	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout.Std())
		defer cancel()
	}

	log.Info("copy started", "from", p.From.Repo, "to", p.To.Repo, "snapshots", p.Snapshots)
	start := time.Now()

	cmd := exec.CommandContext(ctx, r.Restic, args...)
	cmd.Env = BuildEnv(p)
	counter := &copyCounter{}
	stdout := &lineWriter{log: log, stream: "stdout", counter: counter, verbose: r.Verbose}
	stderr := &lineWriter{log: log, stream: "stderr", counter: counter, verbose: r.Verbose}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	stdout.flush()
	stderr.flush()
	res.Duration = time.Since(start)
	res.Seconds = res.Duration.Round(time.Millisecond).Seconds()
	res.Copied, res.Skipped = counter.totals()

	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("timed out after %s", p.Timeout.Std())
		}
		res.Status = "failure"
		res.Error = err.Error()
		if lines := counter.lastLines(); lines != "" {
			res.Error += ": " + lines
		}
		log.Error("copy failed", "error", res.Error, "duration", res.Duration.Round(time.Second))
		return res
	}

	log.Info("copy finished",
		"copied", res.Copied, "skipped", res.Skipped,
		"duration", res.Duration.Round(time.Second))
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
)

func (c *copyCounter) observe(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case savedRe.MatchString(line):
		c.copied++
	case skippedRe.MatchString(line):
		c.skipped++
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

// lineWriter splits process output into lines and logs each one. Write is
// called from exec's copy goroutine; splitting happens synchronously so no
// data is in flight once cmd.Run returns.
type lineWriter struct {
	log     *slog.Logger
	stream  string
	counter *copyCounter
	verbose bool
	buf     []byte
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
	if w.verbose {
		w.log.Info(line, "stream", w.stream)
	} else {
		w.log.Debug(line, "stream", w.stream)
	}
}
