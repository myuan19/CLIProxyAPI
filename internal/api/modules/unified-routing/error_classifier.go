package unifiedrouting

import (
	"context"
	"errors"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// ErrorClass represents the retryability classification of an error.
type ErrorClass int

const (
	// ErrorClassRetryable indicates a node-specific problem.
	// The request should be retried on a different target and
	// the failing target should enter cooldown.
	// Examples: 401 (bad key), 402 (billing), 429 (rate limit), 5xx (server error), network errors.
	ErrorClassRetryable ErrorClass = iota

	// ErrorClassNonRetryable indicates a request-level problem.
	// Retrying on a different target will produce the same failure,
	// so the system should return the error immediately without
	// putting any target into cooldown.
	// Examples: 400 (invalid request body), 413 (payload too large), 422 (unprocessable).
	ErrorClassNonRetryable
)

// String returns a human-readable label for the error class.
func (c ErrorClass) String() string {
	switch c {
	case ErrorClassRetryable:
		return "retryable"
	case ErrorClassNonRetryable:
		return "non_retryable"
	default:
		return "unknown"
	}
}

// ClassifyError determines whether an error is node-specific (retryable on
// another target) or request-level (will fail on every target).
//
// Classification priority:
//  1. Context cancellation — always non-retryable (client gave up).
//  2. auth.Error with explicit Retryable flag — trust the provider.
//  3. HTTP status code from StatusError or auth.Error.
//  4. Error message heuristics for overload / capacity keywords.
//  5. Default: treat as retryable (conservative; prefer retry over silent failure).
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ErrorClassRetryable
	}

	// 1. Client cancelled the request — retrying is pointless.
	if errors.Is(err, context.Canceled) {
		return ErrorClassNonRetryable
	}

	// 2. auth.Error carries a provider-set Retryable flag.
	var authErr *coreauth.Error
	if errors.As(err, &authErr) {
		if authErr.Retryable {
			return ErrorClassRetryable
		}
		// Provider explicitly marked non-retryable — inspect HTTP status to confirm.
		return classifyHTTPStatus(authErr.HTTPStatus, err)
	}

	// 3. Generic StatusError (from executor layer).
	var statusErr cliproxyexecutor.StatusError
	if errors.As(err, &statusErr) {
		return classifyHTTPStatus(statusErr.StatusCode(), err)
	}

	// 4. No structured error info available — look at the message.
	return classifyByMessage(err)
}

// classifyHTTPStatus maps an HTTP status code to an ErrorClass.
// When the status code alone is ambiguous (e.g. 400), the error message
// is inspected for overload/capacity keywords that indicate a node issue.
func classifyHTTPStatus(code int, err error) ErrorClass {
	switch {
	// ---- Request-level problems (non-retryable) ----
	case code == 400:
		// 400 is usually a bad request, but some providers return 400 for
		// overload / capacity issues. Check the message.
		if isOverloadMessage(err.Error()) {
			return ErrorClassRetryable
		}
		return ErrorClassNonRetryable

	case code == 413: // Payload too large
		return ErrorClassNonRetryable

	case code == 422: // Unprocessable entity
		return ErrorClassNonRetryable

	// ---- Node-level problems (retryable) ----
	case code == 401: // Unauthorized — different credential may work
		return ErrorClassRetryable

	case code == 402: // Payment required — different account may work
		return ErrorClassRetryable

	case code == 403: // Forbidden — different credential/account may work
		return ErrorClassRetryable

	case code == 404: // Not found — different node may host the model
		return ErrorClassRetryable

	case code == 429: // Rate limited — different node may have quota
		return ErrorClassRetryable

	case code >= 500: // Server errors — node specific
		return ErrorClassRetryable

	case code == 0: // No HTTP status — fall through to message check
		return classifyByMessage(err)

	default:
		// Other 4xx we haven't explicitly handled — conservatively treat as
		// non-retryable because they typically indicate client errors.
		if code >= 400 && code < 500 {
			return ErrorClassNonRetryable
		}
		return ErrorClassRetryable
	}
}

// classifyByMessage inspects the error string for patterns that hint at
// the error class. This is the fallback when no structured status is available.
func classifyByMessage(err error) ErrorClass {
	msg := strings.ToLower(err.Error())

	// Network / connectivity errors → node-specific.
	networkKeywords := []string{
		"connection refused",
		"connection reset",
		"no such host",
		"i/o timeout",
		"tls handshake",
		"eof",
		"broken pipe",
		"dial tcp",
	}
	for _, kw := range networkKeywords {
		if strings.Contains(msg, kw) {
			return ErrorClassRetryable
		}
	}

	// Overload / capacity → node-specific.
	if isOverloadMessage(msg) {
		return ErrorClassRetryable
	}

	// Default: assume retryable to avoid silently dropping requests.
	return ErrorClassRetryable
}

// isOverloadMessage returns true if the message suggests the failure
// is due to temporary overload or capacity, not a malformed request.
func isOverloadMessage(msg string) bool {
	msg = strings.ToLower(msg)
	overloadKeywords := []string{
		"overloaded",
		"capacity",
		"too many requests",
		"rate limit",
		"resource exhausted",
		"server is busy",
		"temporarily unavailable",
		"service unavailable",
		"quota",
	}
	for _, kw := range overloadKeywords {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}
