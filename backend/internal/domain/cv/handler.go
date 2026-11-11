package cv

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/httpx"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes the cv domain over HTTP.
type Handler struct {
	svc      *Service
	maxBytes int64
	log      *slog.Logger
}

// NewHandler builds the cv handler. maxBytes must match the storage cap, so a client
// gets a clear rejection rather than a stream error from the layer below.
func NewHandler(svc *Service, maxBytes int64, log *slog.Logger) *Handler {
	return &Handler{svc: svc, maxBytes: maxBytes, log: log}
}

// RegisterRoutes mounts the cv routes. All of them require authentication; there is no
// public CV surface.
func (h *Handler) RegisterRoutes(r chi.Router, requireAuth func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireAuth)

		r.Route("/cvs", func(r chi.Router) {
			r.Get("/", h.list)
			// The upload route escapes the global body cap, which is sized for JSON.
			// The storage layer enforces the real limit on the stream.
			r.With(h.uploadBodyLimit).Post("/", h.upload)

			r.Route("/{cvID}", func(r chi.Router) {
				r.Get("/", h.get)
				r.With(h.uploadBodyLimit).Post("/versions", h.uploadVersion)
				r.Get("/download", h.download)
				r.Patch("/", h.rename)
				r.Post("/default", h.setDefault)
				r.Delete("/", h.delete)
			})
		})
	})
}

// uploadBodyLimit replaces the router's JSON-sized body cap with the upload cap.
//
// A little headroom above the configured maximum so the storage layer's own check is
// what rejects an oversized file, producing a precise field error rather than a generic
// "request body too large" from the HTTP layer.
func (h *Handler) uploadBodyLimit(next http.Handler) http.Handler {
	return httpx.MaxBodyBytes(h.maxBytes + (1 << 20))(next)
}

type documentResponse struct {
	ID               string           `json:"id"`
	Label            string           `json:"label"`
	OriginalFilename string           `json:"original_filename,omitempty"`
	ContentType      string           `json:"content_type"`
	SizeBytes        int64            `json:"size_bytes"`
	Source           string           `json:"source"`
	IsDefault        bool             `json:"is_default"`
	VersionCount     int              `json:"version_count"`
	CreatedAt        time.Time        `json:"created_at"`
	LatestVersion    *versionResponse `json:"latest_version,omitempty"`
}

type versionResponse struct {
	ID          string `json:"id"`
	VersionNo   int    `json:"version_no"`
	SizeBytes   int64  `json:"size_bytes"`
	ParseStatus string `json:"parse_status"`
	// Stage is candidate-facing copy for the current pipeline stage. An empty state
	// that names the stage is the difference between waiting and assuming it is broken.
	Stage        string     `json:"stage"`
	InProgress   bool       `json:"in_progress"`
	ParseError   string     `json:"parse_error,omitempty"`
	ErrorDetail  string     `json:"error_detail,omitempty"`
	Retryable    bool       `json:"retryable,omitempty"`
	AVStatus     string     `json:"av_status"`
	CharCount    *int       `json:"char_count,omitempty"`
	PageCount    *int       `json:"page_count,omitempty"`
	IsTailored   bool       `json:"is_tailored"`
	Usable       bool       `json:"usable"`
	UnsafeReason string     `json:"unsafe_reason,omitempty"`
	ParsedAt     *time.Time `json:"parsed_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

func toVersionResponse(v *Version) *versionResponse {
	if v == nil {
		return nil
	}
	out := &versionResponse{
		ID:           v.ID.String(),
		VersionNo:    v.VersionNo,
		SizeBytes:    v.SizeBytes,
		ParseStatus:  string(v.ParseStatus),
		Stage:        v.ParseStatus.Stage(),
		InProgress:   v.ParseStatus.InProgress(),
		ParseError:   v.ParseError,
		AVStatus:     string(v.AVStatus),
		CharCount:    v.CharCount,
		PageCount:    v.PageCount,
		IsTailored:   v.IsTailored(),
		Usable:       v.SafeToUse(),
		UnsafeReason: v.UnsafeReason(),
		ParsedAt:     v.ParsedAt,
		CreatedAt:    v.CreatedAt,
	}
	// Translate the machine-readable failure reason into something the candidate can
	// act on, and say whether retrying could possibly help.
	if v.ParseError != "" {
		reason := ParseFailureReason(v.ParseError)
		out.ErrorDetail = reason.Message()
		out.Retryable = reason.Retryable()
	}
	return out
}

func toDocumentResponse(d *Document) documentResponse {
	return documentResponse{
		ID:               d.ID.String(),
		Label:            d.DisplayLabel(),
		OriginalFilename: d.OriginalFilename,
		ContentType:      d.ContentType,
		SizeBytes:        d.SizeBytes,
		Source:           string(d.Source),
		IsDefault:        d.IsDefault,
		VersionCount:     d.VersionCount,
		CreatedAt:        d.CreatedAt,
		LatestVersion:    toVersionResponse(d.LatestVersion),
	}
}

func principalUserID(r *http.Request) (uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return uuid.Nil, err
	}
	id, perr := uuid.Parse(principal.UserID)
	if perr != nil {
		return uuid.Nil, apperr.Internalf(perr, "principal carries an unparseable user id")
	}
	return id, nil
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUserID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	documents, err := h.svc.List(r.Context(), userID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// An explicitly empty array rather than null, so a client can render the empty
	// state without a null check.
	out := make([]documentResponse, 0, len(documents))
	for _, d := range documents {
		out = append(out, toDocumentResponse(d))
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"cvs": out})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUserID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	documentID, err := uuid.Parse(chi.URLParam(r, "cvID"))
	if err != nil {
		httpx.Error(w, r, apperr.NotFound("CV"))
		return
	}

	document, err := h.svc.Get(r.Context(), userID, documentID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"cv": toDocumentResponse(document)})
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	h.handleUpload(w, r, uuid.Nil)
}

func (h *Handler) uploadVersion(w http.ResponseWriter, r *http.Request) {
	documentID, err := uuid.Parse(chi.URLParam(r, "cvID"))
	if err != nil {
		httpx.Error(w, r, apperr.NotFound("CV"))
		return
	}
	h.handleUpload(w, r, documentID)
}

// handleUpload reads the multipart body and delegates to the service.
//
// The file part is streamed, not buffered: ReadForm would write the whole upload to a
// temporary file first, which turns the size cap into something enforced after the cost
// has already been paid.
func (h *Handler) handleUpload(w http.ResponseWriter, r *http.Request, documentID uuid.UUID) {
	userID, err := principalUserID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	reader, err := r.MultipartReader()
	if err != nil {
		httpx.Error(w, r, apperr.Validation("not_multipart",
			"the request must be multipart/form-data with a 'file' part"))
		return
	}

	var (
		label       string
		makeDefault bool
		result      *UploadResult
	)

	// Metadata parts may arrive before or after the file. Requiring them first would
	// work for a hand-written curl command and fail for any client that builds the
	// body with the file first — which is what most form libraries do. So the file is
	// streamed when it appears, and trailing metadata is applied afterwards.
	for {
		part, perr := reader.NextPart()
		if perr != nil {
			break
		}

		switch part.FormName() {
		case "label":
			label = readShortField(part)
			_ = part.Close()

		case "make_default":
			makeDefault = readShortField(part) == "true"
			_ = part.Close()

		case "file":
			if result != nil {
				_ = part.Close()
				httpx.Error(w, r, apperr.Validation("multiple_files",
					"send exactly one 'file' part"))
				return
			}

			uploaded, uerr := h.svc.Upload(r.Context(), UploadInput{
				UserID:           userID,
				DocumentID:       documentID,
				Label:            label,
				OriginalFilename: part.FileName(),
				DeclaredType:     part.Header.Get("Content-Type"),
				Body:             part,
				MakeDefault:      makeDefault,
			})
			_ = part.Close()

			if uerr != nil {
				httpx.Error(w, r, uerr)
				return
			}
			result = uploaded

		default:
			_ = part.Close()
		}
	}

	if result == nil {
		httpx.ValidationError(w, r, "file", "required", "a 'file' part is required")
		return
	}

	// A label that arrived after the file is applied now. Failing the upload over a
	// label would be absurd, so a failure here is logged and the upload stands.
	if label != "" && result.Document.DisplayLabel() != label {
		if rerr := h.svc.Rename(r.Context(), userID, result.Document.ID, label); rerr != nil {
			h.log.WarnContext(r.Context(), "upload succeeded but the label could not be applied",
				slog.String("cv_id", result.Document.ID.String()), slog.Any("error", rerr))
		} else {
			result.Document.Label = label
		}
	}

	// 202 rather than 201: the document exists, but parsing has not finished, and the
	// client should poll the version's stage.
	httpx.JSON(w, r, http.StatusAccepted, map[string]any{
		"cv":      toDocumentResponse(result.Document),
		"version": toVersionResponse(result.Version),
		"message": "your CV is being analysed",
	})
}

// maxFieldBytes bounds a text form field. These are labels and flags; anything larger
// is a mistake or an attempt to exhaust memory through a part that is not the file.
const maxFieldBytes = 4096

func readShortField(r interface{ Read([]byte) (int, error) }) string {
	buf := make([]byte, maxFieldBytes)
	n, _ := r.Read(buf)
	if n <= 0 {
		return ""
	}
	return string(buf[:n])
}

func (h *Handler) download(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUserID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	documentID, err := uuid.Parse(chi.URLParam(r, "cvID"))
	if err != nil {
		httpx.Error(w, r, apperr.NotFound("CV"))
		return
	}

	url, err := h.svc.DownloadURL(r.Context(), userID, documentID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}

	// The URL is returned rather than redirected to, so the client controls when the
	// download starts and the link is not leaked in a Referer header.
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"download_url": url,
		"expires_in":   300,
	})
}

type renameRequest struct {
	Label string `json:"label" validate:"required,max=120"`
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUserID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	documentID, err := uuid.Parse(chi.URLParam(r, "cvID"))
	if err != nil {
		httpx.Error(w, r, apperr.NotFound("CV"))
		return
	}

	var in renameRequest
	if err := httpx.Bind(r, &in); err != nil {
		httpx.Error(w, r, err)
		return
	}
	if err := h.svc.Rename(r.Context(), userID, documentID, in.Label); err != nil {
		httpx.Error(w, r, err)
		return
	}

	document, err := h.svc.Get(r.Context(), userID, documentID)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"cv": toDocumentResponse(document)})
}

func (h *Handler) setDefault(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUserID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	documentID, err := uuid.Parse(chi.URLParam(r, "cvID"))
	if err != nil {
		httpx.Error(w, r, apperr.NotFound("CV"))
		return
	}

	if err := h.svc.SetDefault(r.Context(), userID, documentID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"message": "default CV updated"})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUserID(r)
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	documentID, err := uuid.Parse(chi.URLParam(r, "cvID"))
	if err != nil {
		httpx.Error(w, r, apperr.NotFound("CV"))
		return
	}

	if err := h.svc.Delete(r.Context(), userID, documentID); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.NoContent(w)
}
