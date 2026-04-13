package sandbox

import "fmt"

// SandboxError represents an error returned by the Sandbox API.
type SandboxError struct {
	StatusCode int    // HTTP status code
	Code       string // server-side error code
	Message    string // human-readable message
}

func (e *SandboxError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("sandbox: %s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("sandbox: HTTP %d: %s", e.StatusCode, e.Message)
}

// Is reports whether target matches this error by StatusCode and Code.
// If target.Code is empty, only StatusCode is compared.
func (e *SandboxError) Is(target error) bool {
	t, ok := target.(*SandboxError)
	if !ok {
		return false
	}
	if e.StatusCode != t.StatusCode {
		return false
	}
	if t.Code != "" && e.Code != t.Code {
		return false
	}
	return true
}

// Predefined sentinel errors for common HTTP status codes.
var (
	ErrNotFound       = &SandboxError{StatusCode: 404, Code: "SANDBOX_NOT_FOUND"}
	ErrUnauthorized   = &SandboxError{StatusCode: 401}
	ErrRateLimited    = &SandboxError{StatusCode: 429}
	ErrTimeout        = &SandboxError{StatusCode: 408}
	ErrInvalidRequest = &SandboxError{StatusCode: 400}
)
