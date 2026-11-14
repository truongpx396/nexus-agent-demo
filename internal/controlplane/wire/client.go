package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/truongpx396/nexus-agent/internal/controlplane"
)

// Client is a controlplane.Port that calls a Handler over HTTP.
type Client struct {
	BaseURL string       // e.g. "http://control-plane:8090"; the /controlplane/v1/ path is added here
	HTTP    *http.Client // nil means http.DefaultClient
}

var _ controlplane.Port = (*Client)(nil)

func (c *Client) AdmitRun(ctx context.Context, req controlplane.AdmitRunV1) (controlplane.AdmitRunResultV1, error) {
	return call[controlplane.AdmitRunV1, controlplane.AdmitRunResultV1](ctx, c, opAdmitRun, req)
}

func (c *Client) ReserveBudget(ctx context.Context, req controlplane.ReserveBudgetV1) (controlplane.ReserveBudgetResultV1, error) {
	return call[controlplane.ReserveBudgetV1, controlplane.ReserveBudgetResultV1](ctx, c, opReserveBudget, req)
}

func (c *Client) ReportCost(ctx context.Context, req controlplane.ReportCostV1) error {
	_, err := call[controlplane.ReportCostV1, struct{}](ctx, c, opReportCost, req)
	return err
}

func (c *Client) EmitAuditReceipt(ctx context.Context, req controlplane.EmitAuditReceiptV1) error {
	_, err := call[controlplane.EmitAuditReceiptV1, struct{}](ctx, c, opEmitAuditReceipt, req)
	return err
}

func (c *Client) RequestApproval(ctx context.Context, req controlplane.RequestApprovalV1) (controlplane.RequestApprovalResultV1, error) {
	return call[controlplane.RequestApprovalV1, controlplane.RequestApprovalResultV1](ctx, c, opRequestApproval, req)
}

func (c *Client) AuthorizeContentAccess(ctx context.Context, req controlplane.AuthorizeContentAccessV1) (controlplane.GrantV1, error) {
	return call[controlplane.AuthorizeContentAccessV1, controlplane.GrantV1](ctx, c, opAuthorizeContentAccess, req)
}

func call[Req, Res any](ctx context.Context, c *Client, op string, req Req) (Res, error) {
	var zero Res
	body, err := json.Marshal(req)
	if err != nil {
		return zero, fmt.Errorf("controlplane %s: encode request: %w", op, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+basePath+op, bytes.NewReader(body))
	if err != nil {
		return zero, fmt.Errorf("controlplane %s: %w", op, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(httpReq)
	if err != nil {
		return zero, fmt.Errorf("controlplane %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return zero, fmt.Errorf("controlplane %s: read response: %w", op, err)
	}
	if resp.StatusCode != http.StatusOK {
		var eb errorBody
		_ = json.Unmarshal(data, &eb) // a non-JSON error body (a proxy's) falls back to the status text
		msg := eb.Error
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return zero, fmt.Errorf("controlplane %s: %s (status %d)", op, msg, resp.StatusCode)
	}
	var res Res
	if err := json.Unmarshal(data, &res); err != nil {
		return zero, fmt.Errorf("controlplane %s: decode response: %w", op, err)
	}
	return res, nil
}
