package etcdgateway

import (
	"errors"
	"net/http"
	"strings"

	"github.com/goairix/sandbox/internal/provisioning"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
)

// destroy shares the original authenticated business admission. A 202 records
// only the existing close/task transaction, never physical cleanup completion.
// The independent automatic worker must support and cover this deployment's
// original namespace, restore epoch, local storage profile and partition.
func (g *Gateway) destroy(w http.ResponseWriter, r *http.Request, id string) {
	for name := range r.Header {
		if strings.EqualFold(name, "Idempotency-Key") {
			writeError(w, 400, "invalid_request", "Idempotency-Key is not supported for destroy")
			return
		}
	}
	requestID, valid := validHeaderValue(r.Header, "X-Request-ID", 128, true)
	if !valid {
		writeError(w, 400, "invalid_request", "invalid X-Request-ID")
		return
	}
	if requestID == "" {
		requestID = uuid.NewString()
	}
	w.Header().Set("X-Request-ID", requestID)

	// One original service call owns preparation, commit/resolution and lease
	// release. Repeating it is a new attempt, including with the same request ID.
	result, err := g.service.Destroy(r.Context(), id, requestID)
	if errors.Is(err, state.ErrOutcomeUnknown) || result.State == provisioning.PlainColdDestroyUnknown {
		writeDestroyResult(w, 503, "outcome_unknown", result)
		return
	}
	if errors.Is(err, provisioning.ErrPlainColdDestroyProfile) {
		writeError(w, 501, "unsupported_cleanup_profile", "sandbox cleanup profile is not supported")
		return
	}
	if errors.Is(err, provisioning.ErrPlainColdDestroyConflict) {
		writeError(w, 409, "destroy_conflict", "sandbox changed or is already closing")
		return
	}
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if result.State != provisioning.PlainColdDestroyPending {
		writeDestroyResult(w, 503, "outcome_unknown", result)
		return
	}
	w.Header().Set("Retry-After", "1")
	writeDestroyResult(w, 202, "destroy_pending", result)
}

// writeDestroyResult deliberately retains bounded original attribution for
// unknown attempts. There is no request polling endpoint or durable HTTP
// idempotency contract, and these references never authorize cleanup effects.
func writeDestroyResult(w http.ResponseWriter, status int, code string, result provisioning.PlainColdDestroyResult) {
	writeJSON(w, status, struct {
		Code       string                             `json:"code"`
		State      provisioning.PlainColdDestroyState `json:"state"`
		SandboxID  string                             `json:"sandbox_id"`
		RequestID  string                             `json:"request_id"`
		Generation int64                              `json:"generation,omitempty"`
		Runtime    p.RuntimeReference                 `json:"runtime"`
		Task       state.TaskReference                `json:"task"`
		Stage      state.StageReference               `json:"stage"`
	}{code, result.State, result.SandboxID, result.RequestID, result.Generation, result.Runtime, result.Task, result.Stage})
}
