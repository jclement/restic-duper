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
