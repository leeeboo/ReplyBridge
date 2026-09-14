// Package bili implements the small portion of Bilibili's web API needed by
// the metadata synchronizer and the explicitly gated comment bridge. It has
// no playurl endpoint or media download API; comment writes use undocumented
// browser endpoints and remain experimental.
package bili

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrRateLimited is returned for HTTP/API 412 responses. Callers should
	// cool down instead of immediately retrying the request.
	ErrRateLimited = errors.New("bilibili: rate limited")
	// ErrAuthentication is returned for Bilibili API codes -101 and -111.
	ErrAuthentication = errors.New("bilibili: authentication required")
	ErrNotFound       = errors.New("bilibili: not found")
)

// APIError is a Bilibili JSON envelope whose code is non-zero.
// It never includes the request URL because URLs can contain sensitive query
// strings (notably subtitle URLs).
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("bilibili API error %d", e.Code)
	}
	return fmt.Sprintf("bilibili API error %d: %s", e.Code, e.Message)
}

func (e *APIError) Is(target error) bool {
	switch target {
	case ErrAuthentication:
		return e.Code == -101 || e.Code == -111
	case ErrRateLimited:
		return e.Code == -412 || e.Code == 412
	case ErrNotFound:
		return e.Code == -404 || e.Code == 62002 || e.Code == 62004
	}
	return false
}

// CooldownError reports a server-requested cooldown. It wraps ErrRateLimited
// so errors.Is(err, ErrRateLimited) remains convenient.
type CooldownError struct {
	RetryAfter time.Duration
}

func (e *CooldownError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%v: retry after %s", ErrRateLimited, e.RetryAfter.Round(time.Second))
	}
	return ErrRateLimited.Error()
}

func (e *CooldownError) Unwrap() error { return ErrRateLimited }
