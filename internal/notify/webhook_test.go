package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jclement/restic-duper/internal/config"
	"github.com/jclement/restic-duper/internal/runner"
)

func init() { retryUnit = 10 * time.Millisecond }

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func webhookFor(url string) *config.Webhook {
	w := &config.Webhook{URL: url, Method: "POST", Timeout: config.Duration(5 * time.Second),
		Headers: map[string]string{"X-Token": "abc"}}
	return w
}

func TestNewPayloadStatus(t *testing.T) {
	ok := runner.Result{Name: "a", Status: "success"}
	bad := runner.Result{Name: "b", Status: "failure", Error: "boom"}

	p := NewPayload("1.0", time.Now(), []runner.Result{ok})
	if p.Status != "success" {
		t.Errorf("status = %q, want success", p.Status)
	}
	p = NewPayload("1.0", time.Now(), []runner.Result{ok, bad})
	if p.Status != "failure" {
		t.Errorf("status = %q, want failure", p.Status)
	}
}

func TestSendSuccess(t *testing.T) {
	var got Payload
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("X-Token")
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	p := NewPayload("1.0", time.Now(), []runner.Result{{Name: "a", Status: "failure", Error: "x"}})
	if err := Send(context.Background(), discard(), webhookFor(srv.URL), p); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if header != "abc" {
		t.Errorf("custom header not sent, got %q", header)
	}
	if got.Tool != "restic-duper" || got.Status != "failure" || len(got.Pairs) != 1 {
		t.Errorf("bad payload: %+v", got)
	}
}

func TestSendRetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}))
	defer srv.Close()

	err := Send(context.Background(), discard(), webhookFor(srv.URL), Payload{})
	if err != nil {
		t.Fatalf("Send should succeed on 3rd attempt: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

// The "events" format must post a JSON array of flat per-pair events with
// _time and level fields (Axiom-style ingest).
func TestSendEventsFormat(t *testing.T) {
	var got []Event
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	w := webhookFor(srv.URL)
	w.Format = "events"
	w.Headers["Authorization"] = "Bearer tok123"

	finished := time.Date(2026, 7, 15, 20, 45, 0, 0, time.UTC)
	p := NewPayload("1.0", time.Now(), []runner.Result{
		{Name: "a", FromRepo: "/src", ToRepo: "azure:c:/p", Status: "success", Seconds: 174, Copied: 1, FinishedAt: finished},
		{Name: "b", Status: "failure", Error: "boom", FinishedAt: finished},
	})
	p.Command = "run"
	if err := Send(context.Background(), discard(), w, p); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if auth != "Bearer tok123" {
		t.Errorf("Authorization = %q", auth)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(got), got)
	}
	e := got[0]
	if e.Time != "2026-07-15T20:45:00Z" || e.Service != "restic-duper" || e.Command != "run" ||
		e.Pair != "a" || e.ToRepo != "azure:c:/p" || e.Level != "info" || e.SnapshotsCopied != 1 {
		t.Errorf("bad event: %+v", e)
	}
	if got[1].Level != "error" || got[1].Error != "boom" {
		t.Errorf("failure event must have level=error: %+v", got[1])
	}
}

func TestEventsSetupFailure(t *testing.T) {
	p := NewPayload("1.0", time.Now(), nil)
	p.Status = "failure"
	p.Error = "cannot run restic"
	evs := Events(p)
	if len(evs) != 1 || evs[0].Level != "error" || evs[0].Error != "cannot run restic" || evs[0].Time == "" {
		t.Errorf("bad setup-failure events: %+v", evs)
	}
}

// A method-changing redirect (301/302/303) must be a delivery failure:
// following it would turn the POST into a GET and silently drop the payload.
func TestSendTreatsMovedRedirectAsFailure(t *testing.T) {
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/final", http.StatusMovedPermanently)
	}))
	defer srv.Close()

	err := Send(context.Background(), discard(), webhookFor(srv.URL), Payload{})
	if err == nil {
		t.Fatal("301 must be reported as a failure")
	}
	if !strings.Contains(err.Error(), "/final") {
		t.Errorf("error should include the redirect target: %v", err)
	}
	if followed.Load() {
		t.Error("301 must not be followed")
	}
}

// 307/308 preserve method and body, so they are followed and the payload
// must arrive intact at the final URL.
func TestSendFollowsTemporaryRedirect(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/final" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	if err := Send(context.Background(), discard(), webhookFor(srv.URL), Payload{Status: "failure"}); err != nil {
		t.Fatalf("307 should be followed: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("method after 307 = %s, want POST", gotMethod)
	}
	if !strings.Contains(gotBody, `"failure"`) {
		t.Errorf("body was dropped across redirect: %q", gotBody)
	}
}

func TestSendGivesUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	err := Send(context.Background(), discard(), webhookFor(srv.URL), Payload{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should mention status: %v", err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}
