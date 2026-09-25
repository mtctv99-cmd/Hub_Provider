package scheduler

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type ErrorKind string

const (
	ErrRateLimited     ErrorKind = "rate_limited"
	ErrAuthExpired     ErrorKind = "auth_expired"
	ErrAuthForbidden   ErrorKind = "auth_forbidden"
	ErrUpstreamTimeout ErrorKind = "upstream_timeout"
	ErrProviderDown    ErrorKind = "provider_down"
	ErrQuotaExhausted  ErrorKind = "quota_exhausted"
	ErrBadRequest      ErrorKind = "bad_request"
	ErrCancelled       ErrorKind = "cancelled"
	ErrInternal        ErrorKind = "internal"
)

type CanonicalError struct {
	Kind       ErrorKind
	StatusCode int
	RetryAfter time.Duration
	Message    string
	Retryable  bool
}

func (e CanonicalError) Error() string { return e.Message }

func Classify(status int, body, retryAfter string) CanonicalError {
	text := strings.ToLower(body)
	if status == http.StatusTooManyRequests || strings.Contains(text, "rate limit") || strings.Contains(text, "too many") {
		return CanonicalError{Kind: ErrRateLimited, StatusCode: status, RetryAfter: parseRetryAfter(retryAfter), Message: "provider rate limited request", Retryable: true}
	}
	if status == http.StatusUnauthorized || strings.Contains(text, "token expired") || strings.Contains(text, "session expired") {
		return CanonicalError{Kind: ErrAuthExpired, StatusCode: status, Message: "provider credential expired", Retryable: false}
	}
	if status == http.StatusForbidden || strings.Contains(text, "captcha") || strings.Contains(text, "bot verification") {
		return CanonicalError{Kind: ErrAuthForbidden, StatusCode: status, Message: "provider denied credential", Retryable: false}
	}
	if status == http.StatusBadRequest {
		return CanonicalError{Kind: ErrBadRequest, StatusCode: status, Message: "provider rejected request", Retryable: false}
	}
	if status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout {
		return CanonicalError{Kind: ErrUpstreamTimeout, StatusCode: status, Message: "provider request timed out", Retryable: true}
	}
	if status >= 500 || status == 0 {
		return CanonicalError{Kind: ErrProviderDown, StatusCode: status, Message: "provider unavailable", Retryable: true}
	}
	return CanonicalError{Kind: ErrInternal, StatusCode: status, Message: "provider request failed", Retryable: false}
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if parsed, err := http.ParseTime(value); err == nil {
		if d := time.Until(parsed); d > 0 {
			return d
		}
	}
	return 0
}
