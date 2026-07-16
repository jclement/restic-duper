package cmd

import (
	"context"
	"log/slog"
	"time"

	"github.com/jclement/restic-duper/internal/config"
	"github.com/jclement/restic-duper/internal/notify"
	"github.com/jclement/restic-duper/internal/runner"
)

// sendNotification delivers the run/forget outcome to the configured
// webhook, honoring on_failure/on_success. It uses a fresh context so a
// Ctrl-C that ended the run cannot also cancel the failure notification.
func sendNotification(log *slog.Logger, cfg *config.Config, command string, started time.Time, results []runner.Result) {
	w := cfg.Notifications.Webhook
	if w == nil {
		return
	}
	payload := notify.NewPayload(version, started, results)
	payload.Command = command
	shouldSend := (payload.Status == "failure" && w.FireOnFailure()) ||
		(payload.Status == "success" && w.OnSuccess)
	if !shouldSend {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := notify.Send(ctx, log, w, payload); err != nil {
		log.Error("notification failed", "error", err)
	}
}
