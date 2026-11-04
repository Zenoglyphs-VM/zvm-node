package verify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// CompilerErrorHeader carries one versioned, fixed failure code. Response text
// is never a trusted diagnostic or an input to client-side error classification.
const CompilerErrorHeader = "X-ZVM-Compiler-Error"
const MaxCompilerVersionBytes = maxCompilerName

var ErrCompilerUnavailable = errors.New("compiler service unavailable")

// CheckCompilerVersion bounds the name before release/cache work or logging.
// Resolve still checks the exact release identity and pinned compiler bytes.
func CheckCompilerVersion(version string) error {
	if len(version) > MaxCompilerVersionBytes || strings.TrimPrefix(strings.TrimSpace(version), "v") == "" {
		return fmt.Errorf("%w: compiler version is empty or too long", ErrInvalid)
	}
	return nil
}

// CompilerHTTPFailure preserves the established HTTP statuses while exporting
// only fixed public codes/messages, even when the original error wraps details.
func CompilerHTTPFailure(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, ErrCompilerCacheFull):
		return http.StatusServiceUnavailable, "v1.cache-full", "compiler capacity is unavailable; try again shortly"
	case errors.Is(err, ErrCompilerBusy):
		return http.StatusServiceUnavailable, "v1.busy", "compiler capacity is unavailable; try again shortly"
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest, "v1.invalid", "invalid compiler request"
	case errors.Is(err, ErrCompilerOutputLimit):
		return http.StatusBadRequest, "v1.output-limit", ErrCompilerOutputLimit.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "v1.deadline", "compiler deadline exceeded"
	default:
		return http.StatusInternalServerError, "v1.unavailable", "compiler execution failed"
	}
}

func compilerResponseError(status int, code string) error {
	if code == "" {
		// The old service has no error-code header. Preserve only its known
		// status classes; never forward or parse its response body.
		switch status {
		case http.StatusBadRequest:
			return ErrInvalid
		case http.StatusServiceUnavailable:
			return ErrCompilerBusy
		case http.StatusGatewayTimeout:
			return context.DeadlineExceeded
		default:
			return ErrCompilerUnavailable
		}
	}
	switch {
	case status == http.StatusServiceUnavailable && code == "v1.cache-full":
		return ErrCompilerCacheFull
	case status == http.StatusServiceUnavailable && code == "v1.busy":
		return ErrCompilerBusy
	case status == http.StatusBadRequest && code == "v1.invalid":
		return ErrInvalid
	case status == http.StatusBadRequest && code == "v1.output-limit":
		return ErrCompilerOutputLimit
	case status == http.StatusGatewayTimeout && code == "v1.deadline":
		return context.DeadlineExceeded
	case status == http.StatusInternalServerError && code == "v1.unavailable":
		return ErrCompilerUnavailable
	default:
		return ErrCompilerUnavailable
	}
}

func compilerTransportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrCompilerUnavailable, context.DeadlineExceeded)
	}
	return ErrCompilerUnavailable
}
