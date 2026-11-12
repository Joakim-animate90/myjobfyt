package cv

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// BuilderHandler exposes the CV builder over HTTP.
type BuilderHandler struct {
	repo Repository
	log  *slog.Logger
}

// NewBuilderHandler builds the handler.
func NewBuilderHandler(repo Repository, log *slog.Logger) *BuilderHandler {
	return &BuilderHandler{repo: repo, log: log}
}

// RegisterRoutes mounts the builder routes.
func (h *BuilderHandler) RegisterRoutes(r chi.Router, requireAuth func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireAuth)
		r.Get("/cvs/templates", h.listTemplates)
		// One route for preview and download. Serving them from separate code paths is
		// how they drift apart, and MJF-012 requires the bytes to be identical; the
		// only difference is the Content-Disposition header.
		r.Get("/cvs/versions/{versionID}/build", h.build)
	})
}

func (h *BuilderHandler) listTemplates(w http.ResponseWriter, r *http.Request) {
	infos := Templates()
	out := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		out = append(out, map[string]any{
			"id":          string(info.ID),
			"description": info.Description,
		})
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"templates": out})
}

func (h *BuilderHandler) build(w http.ResponseWriter, r *http.Request) {
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

	// Taken from the query rather than a body so the same URL can be used as a preview
	// source. Validated against the allowlist inside BuildPDF, not here, so there is
	// one place that decides what may be rendered.
	template := TemplateID(r.URL.Query().Get("template"))
	if template == "" {
		template = TemplateClassic
	}

	parsed, err := h.repo.ParsedDataByVersion(r.Context(), userID, versionID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	skills, err := h.repo.SkillsByVersion(r.Context(), userID, versionID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	pdf, err := BuildPDF(parsed, skills, template)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	disposition := "inline"
	if r.URL.Query().Get("download") == "true" {
		disposition = "attachment"
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(pdf)))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("%s; filename=%q", disposition, DownloadNameFor(parsed)))
	// A generated CV is personal data assembled per request; a shared cache must not
	// keep it.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(http.StatusOK)
	if _, werr := w.Write(pdf); werr != nil {
		// The response has already begun, so there is nothing to tell the client. Worth
		// a log line: a truncated CV download looks to the candidate like a corrupt file.
		h.log.WarnContext(r.Context(), "cv pdf download was interrupted",
			slog.String("cv_version_id", versionID.String()), slog.Any("error", werr))
	}
}

// DownloadNameFor builds a filename from the candidate's name.
//
// Sanitised to a conservative set: the value reaches a Content-Disposition header and
// then a filesystem, and a quote or a path separator in it is a header-injection and a
// path-traversal question at once.
func DownloadNameFor(parsed *ParsedData) string {
	name := "cv"
	if parsed != nil {
		if trimmed := strings.TrimSpace(parsed.Contact.FullName); trimmed != "" {
			name = trimmed
		}
	}

	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_':
			b.WriteByte('-')
		}
	}
	safe := strings.Trim(b.String(), "-")
	if safe == "" {
		safe = "cv"
	}
	if len(safe) > 60 {
		safe = safe[:60]
	}
	return safe + ".pdf"
}
