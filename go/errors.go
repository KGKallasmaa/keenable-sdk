package keenable

// Error is implemented by every error this package returns.
//
// errors.As into Error catches the whole set, the way except KeenableError
// does in the other SDKs. errors.As into a concrete type catches one case.
type Error interface {
	error
	keenableError()
}

// InvalidRequestError means the arguments were rejected locally.
// No request was sent.
type InvalidRequestError struct {
	msg string
}

func (e *InvalidRequestError) Error() string  { return e.msg }
func (e *InvalidRequestError) keenableError() {}

// ConnectionError means the API could not be reached, or the client deadline
// elapsed before the body arrived. A canceled caller context is returned as
// that context's own error, not as a ConnectionError.
type ConnectionError struct {
	msg string
}

func (e *ConnectionError) Error() string  { return e.msg }
func (e *ConnectionError) keenableError() {}

// APIError is a non-2xx response.
//
// StatusCode is the HTTP status. Body is the API's error message when it sent
// one, otherwise empty. AuthError and RateLimitError unwrap to APIError.
type APIError struct {
	StatusCode int
	Body       string
	msg        string
}

func (e *APIError) Error() string  { return e.msg }
func (e *APIError) keenableError() {}

// AuthError means the API key was rejected (HTTP 401 or 403).
type AuthError struct {
	*APIError
}

func (e *AuthError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.APIError
}

// RateLimitError means the rate limit was exceeded (HTTP 429).
//
// Keyless calls share a lower hourly cap. Set KEENABLE_API_KEY to lift it.
// A key is never required to make a request.
type RateLimitError struct {
	*APIError
}

func (e *RateLimitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.APIError
}

var (
	_ Error = (*InvalidRequestError)(nil)
	_ Error = (*ConnectionError)(nil)
	_ Error = (*APIError)(nil)
	_ Error = (*AuthError)(nil)
	_ Error = (*RateLimitError)(nil)
)
