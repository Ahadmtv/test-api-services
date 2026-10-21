package platform

import (
	"errors"
	"fmt"
)

// FieldError describes a single invalid request field.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// AppError is a handled application error mapped to an RFC 7807 problem
// response. Raw SQL errors and stack traces never leave this boundary.
type AppError struct {
	Code        string
	Message     string
	Status      int
	Err         error
	FieldErrors []FieldError
}

func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *AppError) Unwrap() error {
	return e.Err
}

// NewAppError creates a handled application error.
func NewAppError(code, message string, status int, err error) *AppError {
	return &AppError{Code: code, Message: message, Status: status, Err: err}
}

// Unexpected wraps an unhandled error into a generic 500 response. The inner
// error is logged server-side but never exposed to the client.
func Unexpected(err error) *AppError {
	return &AppError{
		Code:    "internal_error",
		Message: "خطای داخلی سرور",
		Status:  500,
		Err:     err,
	}
}

// DatabaseUnavailable returns a 503 error when the database cannot be reached.
func DatabaseUnavailable(err error) *AppError {
	return &AppError{
		Code:    "database_unavailable",
		Message: "داده‌خانه در دسترس نیست",
		Status:  503,
		Err:     err,
	}
}

// RouteNotFound maps the router-level 404 (no route matched) to a handled
// problem response instead of the generic internal error.
func RouteNotFound() *AppError {
	return &AppError{
		Code:    "route_not_found",
		Message: "مسیر درخواستی یافت نشد",
		Status:  404,
	}
}

// MethodNotAllowed maps the router-level 405 to a handled problem response.
func MethodNotAllowed() *AppError {
	return &AppError{
		Code:    "method_not_allowed",
		Message: "روش دسترسی برای این مسیر مجاز نیست",
		Status:  405,
	}
}

var _ = errors.Is
