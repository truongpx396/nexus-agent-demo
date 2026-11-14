// Package wire is a JSON-over-HTTP transport for controlplane.Port: a Handler
// that serves any Port, and a Client that is one. It exists to prove the v1
// contract survives a process boundary (the round-trip tests in this package
// are that proof), which is the part of a control-plane/data-plane split that
// could otherwise rot unnoticed while the only implementation is in-process.
//
// It is loopback-grade on purpose: no authentication, no TLS, no retries. A
// real split puts mutual TLS and a deadline policy in front of it, and that
// belongs to the deployment, not to the contract.
//
// Wire shape: POST /controlplane/v1/{operation} with the operation's V1 request
// as the JSON body; 200 with its V1 result (an empty object for operations
// that return only an error); any other status with {"error": "..."}. A
// refusal that is data (AdmitRunResultV1.Admitted false) is a 200; only a
// failure to do the work is an error status.
package wire

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/truongpx396/nexus-agent/internal/controlplane"
)

const (
	basePath = "/controlplane/v1/"
	maxBody  = 1 << 20 // a V1 request is a few hundred bytes; 1 MiB bounds a hostile or buggy caller

	opAdmitRun               = "admit-run"
	opReserveBudget          = "reserve-budget"
	opReportCost             = "report-cost"
	opEmitAuditReceipt       = "emit-audit-receipt"
	opRequestApproval        = "request-approval"
	opAuthorizeContentAccess = "authorize-content-access"
)

type errorBody struct {
	Error string `json:"error"`
}

// NewHandler serves p over the wire shape in the package comment.
func NewHandler(p controlplane.Port) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+basePath+opAdmitRun, serve(p.AdmitRun))
	mux.Handle("POST "+basePath+opReserveBudget, serve(p.ReserveBudget))
	mux.Handle("POST "+basePath+opReportCost, serve(errOnly(p.ReportCost)))
	mux.Handle("POST "+basePath+opEmitAuditReceipt, serve(errOnly(p.EmitAuditReceipt)))
	mux.Handle("POST "+basePath+opRequestApproval, serve(p.RequestApproval))
	mux.Handle("POST "+basePath+opAuthorizeContentAccess, serve(p.AuthorizeContentAccess))
	return mux
}

// errOnly adapts an operation that returns only an error to the (result, error)
// shape serve and call share; its result is an empty object on the wire.
func errOnly[Req any](f func(context.Context, Req) error) func(context.Context, Req) (struct{}, error) {
	return func(ctx context.Context, req Req) (struct{}, error) { return struct{}{}, f(ctx, req) }
}

func serve[Req, Res any](call func(context.Context, Req) (Res, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req Req
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "decode request: " + err.Error()})
			return
		}
		res, err := call(r.Context(), req)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the status line is already sent; a failed write has nowhere to be reported
}
