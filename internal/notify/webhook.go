// Package notify delivers run results to a webhook.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jclement/restic-duper/internal/config"
	"github.com/jclement/restic-duper/internal/runner"
)

// Payload is the JSON body sent to the webhook.
type Payload struct {
	Tool       string          `json:"tool"` // "restic-duper"
	Version    string          `json:"version"`
	Command    string          `json:"command,omitempty"` // "run" | "forget"
	Host       string          `json:"host"`
	Status     string          `json:"status"`          // "success" | "failure"
	Error      string          `json:"error,omitempty"` // set for setup failures that prevented any pair from running
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
	Pairs      []runner.Result `json:"pairs"`
}

func NewPayload(version string, started time.Time, results []runner.Result) Payload {
	host, _ := os.Hostname()
	p := Payload{
		Tool:       "restic-duper",
		Version:    version,
		Host:       host,
		Status:     "success",
		StartedAt:  started.UTC(),
		FinishedAt: time.Now().UTC(),
		Pairs:      results,
	}
	for _, r := range results {
		if !r.OK() {
			p.Status = "failure"
			break
		}
	}
	return p
}

// Event is one flat record in the "events" webhook format — an array of
// these is what event-ingest APIs like Axiom expect. Field names follow
// common conventions: _time for the event timestamp, level for
// severity-based highlighting.
type Event struct {
	Time             string  `json:"_time"` // RFC3339
	Service          string  `json:"service"`
	Version          string  `json:"version"`
	Command          string  `json:"command,omitempty"` // "run" | "forget"
	Host             string  `json:"host"`
	Pair             string  `json:"pair,omitempty"`
	FromRepo         string  `json:"from_repo,omitempty"`
	ToRepo           string  `json:"to_repo,omitempty"`
	Status           string  `json:"status"` // "success" | "failure"
	Level            string  `json:"level"`  // "info" | "error"
	Error            string  `json:"error,omitempty"`
	DurationSeconds  float64 `json:"duration_seconds"`
	SnapshotsCopied  int     `json:"snapshots_copied"`
	SnapshotsSkipped int     `json:"snapshots_skipped"`
}

// Events flattens a Payload into one event per pair. A run that produced no
// pair results (e.g. a setup failure) yields a single run-level event so
// the failure still reaches the ingest pipeline.
func Events(p Payload) []Event {
	base := Event{
		Service: "restic-duper",
		Version: p.Version,
		Command: p.Command,
		Host:    p.Host,
	}
	level := func(status string) string {
		if status == "failure" {
			return "error"
		}
		return "info"
	}
	if len(p.Pairs) == 0 {
		e := base
		e.Time = p.FinishedAt.Format(time.RFC3339)
		e.Status = p.Status
		e.Level = level(p.Status)
		e.Error = p.Error
		return []Event{e}
	}
	events := make([]Event, 0, len(p.Pairs))
	for _, r := range p.Pairs {
		e := base
		t := r.FinishedAt
		if t.IsZero() {
			t = p.FinishedAt
		}
		e.Time = t.Format(time.RFC3339)
		e.Pair = r.Name
		e.FromRepo = r.FromRepo
		e.ToRepo = r.ToRepo
		e.Status = r.Status
		e.Level = level(r.Status)
		e.Error = r.Error
		e.DurationSeconds = r.Seconds
		e.SnapshotsCopied = r.Copied
		e.SnapshotsSkipped = r.Skipped
		events = append(events, e)
	}
	return events
}

const attempts = 3

// retryUnit scales retry backoff (attempt * retryUnit); tests shrink it.
var retryUnit = 2 * time.Second

// Send delivers the payload, retrying transient failures with backoff.
// With format "events" the body is a JSON array of per-pair events instead
// of a single run object.
func Send(ctx context.Context, log *slog.Logger, w *config.Webhook, p Payload) error {
	var doc any = p
	if w.Format == "events" {
		doc = Events(p)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encoding webhook payload: %w", err)
	}

	client := &http.Client{
		Timeout: w.Timeout.Std(),
		// Never follow redirects: Go would convert the POST to a GET and
		// drop the JSON body, then report 200 — a silently lost
		// notification. A 3xx response is treated as a delivery failure.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var lastErr error
	for i := 1; i <= attempts; i++ {
		lastErr = send(ctx, client, w, body)
		if lastErr == nil {
			log.Info("webhook delivered", "url", w.URL, "status", p.Status)
			return nil
		}
		log.Warn("webhook delivery failed", "attempt", i, "of", attempts, "error", lastErr)
		if i < attempts {
			select {
			case <-time.After(time.Duration(i) * retryUnit):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("webhook delivery failed after %d attempts: %w", attempts, lastErr)
}

func send(ctx context.Context, client *http.Client, w *config.Webhook, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, w.Method, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "restic-duper")
	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}
