package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/go-playground/validator/v10"
)

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	// Report the JSON field name rather than the Go field name, so a client can map
	// a validation error straight onto its own form field.
	v.RegisterTagNameFunc(func(fld reflectStructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" || name == "" {
			return fld.Name
		}
		return name
	})
	return v
}

// Bind decodes a JSON request body into dst and validates it.
//
// Unknown fields are rejected rather than ignored: silently dropping a misspelled field
// turns a client bug into a data bug that surfaces much later.
func Bind(r *http.Request, dst any) error {
	if r.Body == nil {
		return apperr.Validation("empty_body", "a JSON request body is required")
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		switch {
		case errors.Is(err, io.EOF):
			return apperr.Validation("empty_body", "a JSON request body is required")
		default:
			var typeErr *json.UnmarshalTypeError
			if errors.As(err, &typeErr) {
				return apperr.ValidationFields([]apperr.FieldError{{
					Field:   typeErr.Field,
					Code:    "wrong_type",
					Message: fmt.Sprintf("expected %s", typeErr.Type.String()),
				}})
			}
			// encoding/json reports an unknown field as a plain error with no typed
			// form to match on, so the field name has to come out of the message.
			// Worth the string handling: "unknown field role" is actionable, while
			// "not valid JSON" sends a client hunting for a syntax error that is not
			// there.
			if field, ok := unknownFieldName(err); ok {
				return apperr.ValidationFields([]apperr.FieldError{{
					Field:   field,
					Code:    "unknown_field",
					Message: "this field is not accepted by this endpoint",
				}})
			}
			return apperr.Validation("malformed_json", "the request body is not valid JSON").WithInternal(err)
		}
	}

	// A second decode must hit EOF; anything else means the client sent trailing data.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return apperr.Validation("trailing_data", "the request body must contain exactly one JSON object")
	}

	return Validate(dst)
}

// unknownFieldPrefix is the message encoding/json produces for a rejected field. It is
// matched rather than parsed with a regexp because the format is fixed and a prefix
// check degrades safely if a future Go release rewords it: the caller simply falls back
// to the generic malformed-JSON error.
const unknownFieldPrefix = `json: unknown field "`

// unknownFieldName extracts the field name from an unknown-field decode error.
func unknownFieldName(err error) (string, bool) {
	msg := err.Error()
	start := strings.Index(msg, unknownFieldPrefix)
	if start < 0 {
		return "", false
	}
	rest := msg[start+len(unknownFieldPrefix):]
	end := strings.IndexByte(rest, '"')
	if end <= 0 {
		return "", false
	}
	return rest[:end], true
}

// Validate runs struct validation and converts failures into field errors.
func Validate(dst any) error {
	if err := validate.Struct(dst); err != nil {
		var invalid *validator.InvalidValidationError
		if errors.As(err, &invalid) {
			return apperr.Internalf(err, "validator misuse")
		}
		var verrs validator.ValidationErrors
		if errors.As(err, &verrs) {
			fields := make([]apperr.FieldError, 0, len(verrs))
			for _, fe := range verrs {
				fields = append(fields, apperr.FieldError{
					Field:   fe.Field(),
					Code:    fe.Tag(),
					Message: messageForTag(fe),
				})
			}
			return apperr.ValidationFields(fields)
		}
		return apperr.Internal(err)
	}
	return nil
}

func messageForTag(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "this field is required"
	case "email":
		return "must be a valid email address"
	case "min":
		return fmt.Sprintf("must be at least %s", fe.Param())
	case "max":
		return fmt.Sprintf("must be at most %s", fe.Param())
	case "len":
		return fmt.Sprintf("must be exactly %s characters", fe.Param())
	case "oneof":
		return fmt.Sprintf("must be one of: %s", strings.ReplaceAll(fe.Param(), " ", ", "))
	case "url":
		return "must be a valid URL"
	case "uuid", "uuid4":
		return "must be a valid UUID"
	case "e164":
		return "must be a phone number in international format, e.g. +254700000000"
	case "iso3166_1_alpha2":
		return "must be a two-letter ISO country code"
	case "gte":
		return fmt.Sprintf("must be greater than or equal to %s", fe.Param())
	case "lte":
		return fmt.Sprintf("must be less than or equal to %s", fe.Param())
	case "dive":
		return "contains an invalid entry"
	default:
		return fmt.Sprintf("failed the %q constraint", fe.Tag())
	}
}

// Pagination holds validated list parameters.
type Pagination struct {
	Limit  int
	Offset int
}

// DefaultPageLimit and MaxPageLimit bound list endpoints. The maximum matches the
// documented ceiling of comparable public job APIs, which keeps client code portable.
const (
	DefaultPageLimit = 20
	MaxPageLimit     = 100
)

// ParsePagination reads and clamps limit/offset query parameters.
//
// Out-of-range values are clamped rather than rejected: a client asking for 500 items
// wants as many as possible, and failing the request helps nobody. A non-numeric value
// is a genuine client bug and is rejected.
func ParsePagination(r *http.Request) (Pagination, error) {
	p := Pagination{Limit: DefaultPageLimit, Offset: 0}
	q := r.URL.Query()

	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return p, apperr.ValidationFields([]apperr.FieldError{{
				Field: "limit", Code: "wrong_type", Message: "must be an integer",
			}})
		}
		switch {
		case n < 1:
			p.Limit = 1
		case n > MaxPageLimit:
			p.Limit = MaxPageLimit
		default:
			p.Limit = n
		}
	}

	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return p, apperr.ValidationFields([]apperr.FieldError{{
				Field: "offset", Code: "wrong_type", Message: "must be an integer",
			}})
		}
		if n < 0 {
			n = 0
		}
		p.Offset = n
	}

	return p, nil
}
