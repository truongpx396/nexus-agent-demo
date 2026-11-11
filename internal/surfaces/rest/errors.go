package rest

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// ServerError answers a request that failed for a reason the caller can do
// nothing about — a database, queue or dependency error — with a fixed
// "<op>: internal error" body and a 500, and logs the real cause server-side.
// An error's text names hosts, ports, SQL and driver internals, so it belongs in
// the log, never in a response a client (or an attacker holding a valid token)
// reads. op is a short verb phrase ("create run") and must not carry request
// content. Errors that mean something to the caller (a malformed request, a
// state conflict) keep their own status and message and do not come through here.
func ServerError(c *gin.Context, op string, err error) {
	log.Error().Err(err).Str("op", op).Str("method", c.Request.Method).Str("path", c.Request.URL.Path).Msg("rest: request failed")
	Error(c, op+": internal error", http.StatusInternalServerError)
}
