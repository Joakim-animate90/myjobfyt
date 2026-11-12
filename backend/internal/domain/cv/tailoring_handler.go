package cv

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// TailoringHandler exposes CV tailoring over HTTP.
//
// Separate from the main CV handler because it has a different dependency — the
// tailoring service and the AI provider behind it — and a different availability story:
// everything else in this domain keeps working when the provider is down.
type TailoringHandler struct {
	svc *TailoringService
	log *slog.Logger
}

// NewTailoringHandler builds the handler. A nil service disables the route, which is
// how the API runs without AI credentials.
func NewTailoringHandler(svc *TailoringService, log *slog.Logger) *TailoringHandler {
	return &TailoringHandler{svc: svc, log: log}
}

// RegisterRoutes mounts the tailoring preview route.
func (h *TailoringHandler) RegisterRoutes(r chi.Router, requireAuth func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireAuth)
		// A preview, not a save. Tailoring rewrites how a candidate's experience is
		// described in their name, so this returns the proposal and the diff and
		// changes nothing. Accepting it is a separate, explicit act.
		r.Post("/cvs/versions/{versionID}/tailor", h.preview)
	})
}

type tailorRequest struct {
	// JobDescription is attacker-controlled text. It is never interpolated into a
	// prompt's instructions — the AI layer offers no route for that.
	JobDescription string `json:"job_description" validate:"required,min=50,max=20000"`
}

func (h *TailoringHandler) preview(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		httpx.Error(w, r, apperr.Unprocessable("tailoring_unavailable",
			"CV tailoring is not available on this deployment."))
		return
	}

	userID, err := principalUserID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	versionID, err := uuid.Parse(chi.URLParam(r, "versionID"))
	if err != nil {
		httpx.Error(w, r, apperr.NotFound("CV version"))
		return
	}

	var in tailorRequest
	if err := httpx.Bind(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}

	outcome, err := h.svc.TailorForJob(r.Context(), userID, versionID, in.JobDescription)
	if err != nil {
		// A refusal is not an error the client should retry, and it carries information
		// the candidate is entitled to: what the platform declined to claim for them.
		if isTailoringRefusal(err) {
			httpx.JSON(w, r, http.StatusUnprocessableEntity, map[string]any{
				"error": map[string]any{
					"code": "tailoring_unsafe",
					"message": "We could not tailor this CV for that role without claiming " +
						"something your CV does not say, so we left it unchanged.",
				},
				"rejected": toViolationResponses(outcome.Containment.Violations),
			})
			return
		}
		httpx.Error(w, r, err)
		return
	}

	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"tailored": toTailoredResponse(outcome.Tailored),
		// The diff is the point of this endpoint. The candidate approves the change,
		// which they cannot do without seeing what it is.
		"diff":               toDiffResponse(outcome.Diff),
		"containment_passed": outcome.Containment.Passed,
		"attempts":           outcome.Attempts,
	})
}

// isTailoringRefusal reports whether this is the containment refusal rather than a
// transient failure.
func isTailoringRefusal(err error) bool {
	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		return false
	}
	return appErr.Code == "tailoring_unsafe"
}

type violationResponse struct {
	Kind   string `json:"kind"`
	Field  string `json:"field"`
	Value  string `json:"value"`
	Detail string `json:"detail"`
}

func toViolationResponses(violations []Violation) []violationResponse {
	out := make([]violationResponse, 0, len(violations))
	for _, v := range violations {
		out = append(out, violationResponse{
			Kind: string(v.Kind), Field: v.Field, Value: v.Value, Detail: v.Detail,
		})
	}
	return out
}

type changeResponse struct {
	Kind   string `json:"kind"`
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
	Reason string `json:"reason,omitempty"`
}

func toDiffResponse(diff TailoringDiff) map[string]any {
	changes := make([]changeResponse, 0, len(diff.Changes))
	for _, c := range diff.Changes {
		changes = append(changes, changeResponse{
			Kind: string(c.Kind), Field: c.Field,
			Before: c.Before, After: c.After, Reason: c.Reason,
		})
	}
	return map[string]any{
		"changes":  changes,
		"rejected": toViolationResponses(diff.Rejected),
	}
}

// toTailoredResponse renders the proposed CV.
func toTailoredResponse(parsed *ParsedData) map[string]any {
	if parsed == nil {
		return nil
	}
	return map[string]any{
		"headline":       parsed.Contact.Headline,
		"summary":        parsed.Summary,
		"experience":     parsed.Experience,
		"education":      parsed.Education,
		"certifications": parsed.Certifications,
		"skills":         parsed.ClaimedSkills,
	}
}
