package zalo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// roundTripFunc mirrors internal/surfaces/telegram's own (its doc
// comment) — captures the outgoing request without any real network call.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestSender_Send_RendersEachNotificationKind mirrors
// internal/surfaces/telegram's own (its doc comment).
func TestSender_Send_RendersEachNotificationKind(t *testing.T) {
	cases := []struct {
		name    string
		payload notificationPayload
		want    string
	}{
		{
			name:    "approval",
			payload: notificationPayload{Kind: "approval", SessionID: "s-1", ToolID: "platform/shell@1"},
			want:    "Approval needed for session s-1 (tool: platform/shell@1) — review it in the run's own approval endpoint.",
		},
		{
			name:    "content",
			payload: notificationPayload{Kind: "content", Text: "here is my reply"},
			want:    "here is my reply",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody struct {
				Message struct {
					Text string `json:"text"`
				} `json:"message"`
			}
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				b, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("read request body: %v", err)
				}
				if err := json.Unmarshal(b, &gotBody); err != nil {
					t.Fatalf("unmarshal send body: %v", err)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil))}, nil
			})}

			sender := &Sender{Channels: fakeChannels{}, TenantID: uuid.New(), Client: client}
			payload, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			if err := sender.Send(context.Background(), "zalo", "user-1", payload); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if gotBody.Message.Text != tc.want {
				t.Fatalf("sent text = %q, want %q", gotBody.Message.Text, tc.want)
			}
		})
	}
}
