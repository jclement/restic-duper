package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jclement/restic-duper/internal/runner"
)

// useProgress decides whether the live terminal renderer should be active:
// only on a real character device, never for --json/--quiet, and honoring
// TERM=dumb and --no-progress.
func useProgress(f *os.File) bool {
	if flagJSON || flagQuiet || flagNoProgress {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func useColor() bool {
	return os.Getenv("NO_COLOR") == ""
}

const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiGreen = "\x1b[32m"
	ansiRed   = "\x1b[31m"
	ansiCyan  = "\x1b[36m"
	ansiClear = "\r\x1b[2K" // return to column 0, erase line
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// progressRenderer maintains one live status line at the bottom of the
// output. Completed pairs and log lines are printed permanently above it.
type progressRenderer struct {
	mu    sync.Mutex
	w     io.Writer
	color bool

	active   bool // a live line is currently displayed
	label    string
	start    time.Time
	snapshot string
	percent  float64
	done     int
	total    int
	unit     string
	frame    int

	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func newProgressRenderer(w io.Writer, color bool) *progressRenderer {
	p := &progressRenderer{w: w, color: color, stop: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.mu.Lock()
				p.frame++
				p.redrawLocked()
				p.mu.Unlock()
			case <-p.stop:
				return
			}
		}
	}()
	return p
}

func (p *progressRenderer) paint(color, s string) string {
	if !p.color {
		return s
	}
	return color + s + ansiReset
}

// StartPair begins a live line for one pair.
func (p *progressRenderer) StartPair(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = true
	p.label = label
	p.start = time.Now()
	p.snapshot, p.percent, p.done, p.total, p.unit = "", 0, 0, 0, ""
	p.redrawLocked()
}

// Event updates the live line from a runner progress event.
func (p *progressRenderer) Event(ev runner.ProgressEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ev.SnapshotID != "" {
		p.snapshot = ev.SnapshotID
		p.percent, p.done, p.total, p.unit = 0, 0, 0, ""
	} else {
		p.percent, p.done, p.total, p.unit = ev.Percent, ev.Done, ev.Total, ev.Unit
	}
	p.redrawLocked()
}

// FinishPair replaces the live line with a permanent result line.
func (p *progressRenderer) FinishPair(ok bool, detail string, dur time.Duration) {
	mark, color := "✓", ansiGreen
	if !ok {
		mark, color = "✗", ansiRed
	}
	p.mu.Lock()
	p.active = false // before printing, so the ticker can't redraw a stale frame
	label := p.label
	p.mu.Unlock()
	p.Println(fmt.Sprintf("%s %s  %s %s",
		p.paint(color, mark), label, truncate(detail, 70),
		p.paint(ansiDim, "("+dur.Round(100*time.Millisecond).String()+")")))
}

// Summary prints the final line and shuts the renderer down.
func (p *progressRenderer) Summary(ok bool, s string) {
	color := ansiGreen
	if !ok {
		color = ansiRed
	}
	p.Println(p.paint(color, s))
	p.Close()
}

// Println prints a permanent line above the live line.
func (p *progressRenderer) Println(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprint(p.w, ansiClear+s+"\n")
	p.redrawLocked()
}

// Close stops the render loop and clears the live line. Safe to call twice.
func (p *progressRenderer) Close() {
	p.closeOnce.Do(func() {
		close(p.stop)
		p.wg.Wait()
		p.mu.Lock()
		defer p.mu.Unlock()
		p.active = false
		fmt.Fprint(p.w, ansiClear)
	})
}

// LogWriter returns a writer that routes slog output through the renderer
// so log lines interleave cleanly with the live line.
func (p *progressRenderer) LogWriter() io.Writer { return &rendererLogWriter{p: p} }

type rendererLogWriter struct {
	p   *progressRenderer
	buf []byte
}

func (w *rendererLogWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.p.Println(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}

func (p *progressRenderer) redrawLocked() {
	if !p.active {
		return
	}
	spin := spinnerFrames[p.frame%len(spinnerFrames)]
	elapsed := time.Since(p.start).Round(time.Second)

	var mid string
	switch {
	case p.total > 0:
		mid = fmt.Sprintf("%s %5.1f%%  %d/%d %s",
			p.paint(ansiCyan, renderBar(p.percent, 22)), p.percent, p.done, p.total, p.unit)
	case p.snapshot != "":
		mid = p.paint(ansiDim, "snapshot "+p.snapshot)
	default:
		mid = p.paint(ansiDim, "starting…")
	}
	fmt.Fprint(p.w, ansiClear+fmt.Sprintf("%s %s  %s  %s",
		p.paint(ansiCyan, spin), p.label, mid, p.paint(ansiDim, fmtElapsed(elapsed))))
}

// renderBar draws a fixed-width unicode bar for percent (0-100).
func renderBar(percent float64, width int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := int(percent / 100 * float64(width))
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func fmtElapsed(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, (s%3600)/60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// truncate shortens s to max runes, ellipsizing the middle so both the
// start and the end (often the informative part) survive.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	half := (max - 1) / 2
	return string(r[:half]) + "…" + string(r[len(r)-(max-1-half):])
}

// pairLabel renders "name  from → to" with repos middle-truncated.
func pairLabel(name, from, to string) string {
	return fmt.Sprintf("%s  %s → %s", name,
		truncate(runner.RedactRepo(from), 28), truncate(runner.RedactRepo(to), 28))
}
