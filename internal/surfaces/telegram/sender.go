package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Sender implements surfaces.Sender by POSTing to Telegram's Bot API
// sendMessage endpoint, using the tenant's own sealed bot token — resolved
// per send, never cached across sends (BotTokenFor mirrors ChannelPort.
// BotToken but keyed by the recipient's own tenant, since surfaces.Sender's
// interface carries no tenant parameter of its own).
type Sender struct {
	Channels ChannelPort
	TenantID uuid.UUID // the ONE tenant this Sender instance sends on behalf of — cmd/nexusd constructs one per outbound delivery, matching Outbox.Deliver's own per-call shape
	Client   *http.Client
}

// Send implements surfaces.Sender. recipient is the Telegram chat_id
// (webhook.go's own chatID) — payload is the notification body
// deliverApprovalNotification-equivalent code already built; here it is
// rendered as the message text sent to the chat.
func (s *Sender) Send(ctx context.Context, surfaceID, recipient string, payload []byte) error {
	token, err := s.Channels.BotToken(ctx, s.TenantID)
	if err != nil {
		return fmt.Errorf("telegram: resolve bot token: %w", err)
	}

	var body struct {
		SessionID string `json:"session_id"`
		ToolID    string `json:"tool_id"`
	}
	text := string(payload)
	if err := json.Unmarshal(payload, &body); err == nil {
		text = fmt.Sprintf("Approval needed for session %s (tool: %s) — review it in the run's own approval endpoint.", body.SessionID, body.ToolID)
	}

	reqBody, err := json.Marshal(map[string]string{"chat_id": recipient, "text": text})
	if err != nil {
		return fmt.Errorf("telegram: marshal sendMessage body: %w", err)
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", token)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("telegram: build sendMessage request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("telegram: sendMessage: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body; nothing to flush
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram: sendMessage returned status %d", resp.StatusCode)
	}
	return nil
}
