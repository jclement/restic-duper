package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// Routine logs must go to stdout, warnings and errors to stderr.
func TestConsoleHandlerSplitsStreams(t *testing.T) {
	var out, errOut strings.Builder
	log := slog.New(newConsoleHandler(&out, &errOut, slog.LevelDebug))

	log.Info("routine")
	log.Debug("detail")
	log.Warn("watch out")
	log.Error("boom")

	if !strings.Contains(out.String(), "routine") || !strings.Contains(out.String(), "detail") {
		t.Errorf("stdout missing info/debug:\n%s", out.String())
	}
	if strings.Contains(out.String(), "boom") || strings.Contains(out.String(), "watch out") {
		t.Errorf("warnings/errors leaked to stdout:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "watch out") || !strings.Contains(errOut.String(), "boom") {
		t.Errorf("stderr missing warn/error:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "routine") {
		t.Errorf("info leaked to stderr:\n%s", errOut.String())
	}
}

func TestSplitHandlerJSON(t *testing.T) {
	var out, errOut strings.Builder
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	h := splitHandler{
		low:  slog.NewJSONHandler(&out, opts),
		high: slog.NewJSONHandler(&errOut, opts),
	}
	log := slog.New(h.WithAttrs([]slog.Attr{slog.String("pair", "x")}))
	log.Info("ok")
	log.Error("bad")

	var rec map[string]any
	if err := json.Unmarshal([]byte(out.String()), &rec); err != nil || rec["msg"] != "ok" || rec["pair"] != "x" {
		t.Errorf("stdout JSON record wrong: %s (%v)", out.String(), err)
	}
	if err := json.Unmarshal([]byte(errOut.String()), &rec); err != nil || rec["msg"] != "bad" {
		t.Errorf("stderr JSON record wrong: %s (%v)", errOut.String(), err)
	}
	if !h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled must consider both handlers")
	}
}
