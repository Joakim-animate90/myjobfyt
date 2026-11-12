package cv

import (
	"log/slog"
	"net/http"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// CheckHandler exposes the CV checker over HTTP.
type CheckHandler struct {
	svc *CheckService
	log *slog.Logger
}

// NewCheckHandler builds the handler.
func NewCheckHandler(svc *CheckService, log *slog.Logger) *CheckHandler {
	return &CheckHandler{svc: svc, log: log}
}

// RegisterRoutes mounts the check route.
func (h *CheckHandler) RegisterRoutes(r chi.Router, requireAuth func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireAuth)
		r.Post("/cvs/versions/{versionID}/check", h.check)
	})
}

type checkRequest struct {
	// TargetRole is optional. Without it the deterministic rubric still runs and the
	// report says keyword coverage was not assessed, rather than the request failing.
	TargetRole string `json:"target_role" validate:"omitempty,max=20000"`
}

func (h *CheckHandler) check(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		httpx.Error(w, r, apperr.Unprocessable("check_unavailable",
			"The CV checker is not available on this deployment."))
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

	var in checkRequest
	if err := httpx.Bind(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}

	report, err := h.svc.Check(r.Context(), userID, versionID, in.TargetRole)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	httpx.JSON(w, r, http.StatusOK, map[string]any{"report": toCheckResponse(report)})
}

// toCheckResponse renders a report.
//
// Coverage, degraded and missing are first-class in the response rather than implied:
// a client that showed the overall score without them would present a partial
// assessment as a complete one, which is the failure this whole design avoids.
func toCheckResponse(report CheckReport) map[string]any {
	categories := make([]map[string]any, 0, len(report.Categories))
	for _, c := range report.Categories {
		entry := map[string]any{
			"category": string(c.Category),
			"weight":   c.Weight,
			"assessed": c.Assessed,
		}
		if c.Assessed {
			entry["score"] = c.Score
		} else {
			entry["reason"] = c.Reason
		}
		categories = append(categories, entry)
	}

	findings := make([]map[string]any, 0, len(report.Findings))
	for _, f := range report.Findings {
		findings = append(findings, map[string]any{
			"category":   string(f.Category),
			"severity":   string(f.Severity),
			"location":   f.Location,
			"message":    f.Message,
			"suggestion": f.Suggestion,
		})
	}

	missing := make([]string, 0, len(report.Missing))
	for _, m := range report.Missing {
		missing = append(missing, string(m))
	}

	return map[string]any{
		"overall":        report.Overall,
		"coverage":       report.Coverage,
		"degraded":       report.Degraded,
		"missing":        missing,
		"categories":     categories,
		"findings":       findings,
		"rubric_version": report.RubricVersion,
	}
}
