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

const attempts = 3

// retryUnit scales retry backoff (attempt * retryUnit); tests shrink it.
var retryUnit = 2 * time.Second

// Send delivers the payload, retrying transient failures with backoff.
func Send(ctx context.Context, log *slog.Logger, w *config.Webhook, p Payload) error {
	body, err := json.Marshal(p)
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
