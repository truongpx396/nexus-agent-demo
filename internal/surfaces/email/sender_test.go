package email

import (
	"encoding/json"
	"testing"
)

// TestRenderNotification_RendersEachNotificationKind mirrors
// internal/surfaces/telegram's own TestSender_Send_RendersEachNotificationKind
// (its doc comment) — this package's own Send has no client-injection
// point (renderNotification's own doc comment explains why), so this
// exercises the discriminator directly instead of through a live send.
func TestRenderNotification_RendersEachNotificationKind(t *testing.T) {
	cases := []struct {
		name        string
		payload     notificationPayload
		wantSubject string
		wantText    string
	}{
		{
			name:        "approval",
			payload:     notificationPayload{Kind: "approval", SessionID: "s-1", ToolID: "platform/shell@1"},
			wantSubject: "Approval needed",
			wantText:    "Approval needed for session s-1 (tool: platform/shell@1) — review it in the run's own approval endpoint.",
		},
		{
			name:        "content",
			payload:     notificationPayload{Kind: "content", Text: "here is my reply"},
			wantSubject: "Re: your message",
			wantText:    "here is my reply",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			subject, text := renderNotification(payload)
			if subject != tc.wantSubject {
				t.Errorf("subject = %q, want %q", subject, tc.wantSubject)
			}
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
		})
	}
}
