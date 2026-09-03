package zalo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Sender implements surfaces.Sender by POSTing to Zalo OA's send-message
// API, using the tenant's own current access token — resolved per send,
// constructed per-delivery (webhook.go's own doc comment on why).
type Sender struct {
	Channels ChannelPort
	TenantID uuid.UUID
	Client   *http.Client
}

// Send implements surfaces.Sender. recipient is the Zalo user id
// (webhook.go's own recipientID).
func (s *Sender) Send(ctx context.Context, surfaceID, recipient string, payload []byte) error {
	token, err := s.Channels.AccessToken(ctx, s.TenantID)
	if err != nil {
		return fmt.Errorf("zalo: resolve access token: %w", err)
	}

	var body struct {
		SessionID string `json:"session_id"`
		ToolID    string `json:"tool_id"`
	}
	text := string(payload)
	if err := json.Unmarshal(payload, &body); err == nil {
		text = fmt.Sprintf("Approval needed for session %s (tool: %s) — review it in the run's own approval endpoint.", body.SessionID, body.ToolID)
	}

	reqBody, err := json.Marshal(map[string]any{
		"recipient": map[string]string{"user_id": recipient},
		"message":   map[string]string{"text": text},
	})
	if err != nil {
		return fmt.Errorf("zalo: marshal send body: %w", err)
	}
	const sendURL = "https://openapi.zalo.me/v3.0/oa/message/cs"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, sendURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("zalo: build send request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("access_token", token)

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("zalo: send: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body; nothing to flush
	if resp.StatusCode >= 300 {
		return fmt.Errorf("zalo: send returned status %d", resp.StatusCode)
	}
	return nil
}
