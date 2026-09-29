package openresponses

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Error types from the spec's error table.
const (
	ErrServer          = "server_error"
	ErrInvalidRequest  = "invalid_request"
	ErrNotFound        = "not_found"
	ErrModel           = "model_error"
	ErrTooManyRequests = "too_many_requests"
)

// APIError is an error the proxy returns to the client in the spec's error
// envelope.
type APIError struct {
	Status  int     `json:"-"`
	Type    string  `json:"type"`
	Code    *string `json:"code"`
	Message string  `json:"message"`
	Param   *string `json:"param"`
}

func (e *APIError) Error() string { return fmt.Sprintf("%s (%d): %s", e.Type, e.Status, e.Message) }

// CodeString returns the error code, or the type when there is no code.
func (e *APIError) CodeString() string {
	if e.Code != nil {
		return *e.Code
	}
	return e.Type
}

func NewError(status int, typ, code, message, param string) *APIError {
	e := &APIError{Status: status, Type: typ, Message: message}
	if code != "" {
		e.Code = &code
	}
	if param != "" {
		e.Param = &param
	}
	return e
}

func InvalidRequest(code, message, param string) *APIError {
	return NewError(http.StatusBadRequest, ErrInvalidRequest, code, message, param)
}

func ServerError(code, message string) *APIError {
	return NewError(http.StatusInternalServerError, ErrServer, code, message, "")
}

// WriteError writes the JSON error envelope.
func WriteError(w http.ResponseWriter, e *APIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(struct {
		Error *APIError `json:"error"`
	}{e})
}
