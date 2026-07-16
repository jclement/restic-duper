package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// consoleHandler is a compact human-readable slog handler:
//
//	15:04:05 INFO  copy finished pair=local-to-b2 copied=1
//
// Records below WARN go to out (stdout), WARN and above to errOut (stderr),
// so `restic-duper run > run.log 2> err.log` separates routine logging from
// real problems.
type consoleHandler struct {
	mu     *sync.Mutex
	out    io.Writer
	errOut io.Writer
	level  slog.Level
	attrs  []slog.Attr
}

func newConsoleHandler(out, errOut io.Writer, level slog.Level) *consoleHandler {
	return &consoleHandler{mu: &sync.Mutex{}, out: out, errOut: errOut, level: level}
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format(time.TimeOnly))
	b.WriteByte(' ')
	b.WriteString(fmt.Sprintf("%-5s", r.Level.String()))
	b.WriteByte(' ')
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		writeAttr(&b, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, a)
		return true
	})
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.out
	if r.Level >= slog.LevelWarn {
		w = h.errOut
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeAttr(b *strings.Builder, a slog.Attr) {
	b.WriteByte(' ')
	b.WriteString(a.Key)
	b.WriteByte('=')
	v := a.Value.String()
	if strings.ContainsAny(v, " \t") {
		fmt.Fprintf(b, "%q", v)
	} else {
		b.WriteString(v)
	}
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	nh := *h
	nh.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &nh
}

func (h *consoleHandler) WithGroup(string) slog.Handler { return h }

// splitHandler routes records below WARN to low and WARN+ to high; used to
// split JSON logs between stdout and stderr.
type splitHandler struct {
	low, high slog.Handler
}

func (h splitHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.low.Enabled(ctx, l) || h.high.Enabled(ctx, l)
}

func (h splitHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		return h.high.Handle(ctx, r)
	}
	return h.low.Handle(ctx, r)
}

func (h splitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return splitHandler{low: h.low.WithAttrs(attrs), high: h.high.WithAttrs(attrs)}
}

func (h splitHandler) WithGroup(g string) slog.Handler {
	return splitHandler{low: h.low.WithGroup(g), high: h.high.WithGroup(g)}
}
