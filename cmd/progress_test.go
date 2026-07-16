package cmd

import (
	"strings"
	"testing"
	"time"
)

func TestRenderBar(t *testing.T) {
	if got := renderBar(0, 10); got != "░░░░░░░░░░" {
		t.Errorf("0%% = %q", got)
	}
	if got := renderBar(100, 10); got != "██████████" {
		t.Errorf("100%% = %q", got)
	}
	if got := renderBar(50, 10); got != "█████░░░░░" {
		t.Errorf("50%% = %q", got)
	}
	if got := renderBar(150, 10); got != "██████████" {
		t.Errorf("overflow must clamp: %q", got)
	}
	if got := renderBar(-5, 10); got != "░░░░░░░░░░" {
		t.Errorf("negative must clamp: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 20); got != "short" {
		t.Errorf("no-op truncate = %q", got)
	}
	long := "abcdefghijklmnopqrstuvwxyz"
	got := truncate(long, 11)
	if len([]rune(got)) != 11 || !strings.Contains(got, "…") {
		t.Errorf("truncate = %q (len %d)", got, len([]rune(got)))
	}
	if !strings.HasPrefix(got, "abc") || !strings.HasSuffix(got, "xyz") {
		t.Errorf("middle-ellipsis expected: %q", got)
	}
}

func TestFmtElapsed(t *testing.T) {
	if got := fmtElapsed(75 * time.Second); got != "1:15" {
		t.Errorf("75s = %q", got)
	}
	if got := fmtElapsed(2*time.Hour + 3*time.Minute + 4*time.Second); got != "2:03:04" {
		t.Errorf("2h3m4s = %q", got)
	}
}

// The renderer must clear its live line when printing permanent lines and
// leave no live residue after Close.
func TestRendererInterleave(t *testing.T) {
	var buf strings.Builder
	p := newProgressRenderer(&syncWriter{b: &buf}, false)
	p.StartPair("[1/1] test")
	p.Println("a log line")
	p.FinishPair(true, "copied 1, skipped 0", 1500*time.Millisecond)
	p.Summary(true, "1 succeeded, 0 failed")
	out := buf.String()
	for _, want := range []string{"a log line", "✓ [1/1] test", "copied 1, skipped 0", "1 succeeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, ansiClear) {
		t.Errorf("output must end with a cleared live line")
	}
}

type syncWriter struct{ b *strings.Builder }

func (w *syncWriter) Write(p []byte) (int, error) { return w.b.Write(p) }
