// Package respond centralizes the JSON response envelope so every handler
// in internal/httpapi/handlers produces the same shape.
package respond

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/racetify/racetify-api/internal/domain"
)

type envelope struct {
	Data  any        `json:"data,omitempty"`
	Error *errorBody `json:"error,omitempty"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Field names the request field a validation error concerns, when there
	// is one, so a form can point at it.
	Field string `json:"field,omitempty"`
}

func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if data == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(envelope{Data: data}); err != nil {
		slog.Error("respond: encode json", "error", err)
	}
}

func Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Error: &errorBody{Code: code, Message: message}})
}

// ErrorWithField is Error for a failure tied to one request field.
func ErrorWithField(w http.ResponseWriter, status int, code, message, field string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Error: &errorBody{Code: code, Message: message, Field: field}})
}

// FromServiceError maps the domain-level sentinel errors every service
// returns onto an HTTP status + stable machine-readable code, so handlers
// don't each re-implement this switch.
func FromServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		Error(w, http.StatusNotFound, "not_found", "The requested resource was not found.")
	case errors.Is(err, domain.ErrAlreadyExists):
		Error(w, http.StatusConflict, "already_exists", "The resource already exists.")
	case errors.Is(err, domain.ErrInvalidCredentials):
		Error(w, http.StatusUnauthorized, "invalid_credentials", "Invalid credentials.")
	case errors.Is(err, domain.ErrAccountSuspended):
		Error(w, http.StatusForbidden, "account_suspended", "This account has been suspended.")
	case errors.Is(err, domain.ErrForbidden):
		Error(w, http.StatusForbidden, "forbidden", "You do not have permission to perform this action.")
	case errors.Is(err, domain.ErrInvalidScope):
		Error(w, http.StatusBadRequest, "invalid_scope", err.Error())
	case errors.Is(err, domain.ErrTokenExpired):
		Error(w, http.StatusBadRequest, "token_expired", "This token has expired.")
	case errors.Is(err, domain.ErrTokenConsumed):
		Error(w, http.StatusBadRequest, "token_consumed", "This token has already been used.")
	case errors.Is(err, domain.ErrInvalidOTP):
		Error(w, http.StatusBadRequest, "invalid_otp", "The verification code is invalid or has expired.")
	case errors.Is(err, domain.ErrRateLimited):
		Error(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Please wait a moment and try again.")
	case errors.Is(err, domain.ErrTermsNotAccepted):
		Error(w, http.StatusBadRequest, "terms_not_accepted", "You must accept the Terms of Service and Privacy Policy.")
	case errors.Is(err, domain.ErrInvalidState):
		Error(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		slog.Error("httpapi: unhandled service error", "error", err)
		Error(w, http.StatusInternalServerError, "internal_error", "Something went wrong. Please try again.")
	}
}
