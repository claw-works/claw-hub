// Package notify sends outgoing webhook notifications when tasks complete.
// Hub calls this package; it never imports Discord or Feishu SDKs directly.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// TaskEvent is the payload POSTed to the webhook URL on task completion.
type TaskEvent struct {
	Event           string         `json:"event"` // "task.done" | "task.failed"
	TaskID          string         `json:"task_id"`
	TaskTitle       string         `json:"task_title"`
	Status          string         `json:"status"`
	Result          string         `json:"result,omitempty"`
	Error           string         `json:"error,omitempty"`
	AssignedAgentID string         `json:"assigned_agent_id,omitempty"`
	CompletedAt     time.Time      `json:"completed_at"`
	// Opaque metadata forwarded from ReportChannel so the webhook receiver
	// knows which Discord thread / Feishu chat to post into.
	DiscordThreadID string `json:"discord_thread_id,omitempty"`
	FeishuChatID    string `json:"feishu_chat_id,omitempty"`
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// Send POSTs a TaskEvent to webhookURL. Errors are logged but not returned
// to avoid breaking the task-completion response.
func Send(ctx context.Context, webhookURL string, evt TaskEvent) error {
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("notify: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claw-hub/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("notify: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: webhook returned %d", resp.StatusCode)
	}
	log.Printf("notify: webhook %s → %d", webhookURL, resp.StatusCode)
	return nil
}
