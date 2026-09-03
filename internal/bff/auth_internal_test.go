package bff

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	workos "github.com/workos/workos-go/v10"
)

func TestLogSessionRefreshFailureEmitsOnlySafeWorkOSFields(t *testing.T) {
	var output bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(original) })

	logSessionRefreshFailure(
		&workos.RefreshSessionResult{Reason: "refresh_failed"},
		&workos.APIError{
			StatusCode:       401,
			ErrorCode:        "invalid_client",
			Message:          "must-not-be-logged",
			ErrorDescription: "also-must-not-be-logged",
			RawBody:          "refresh_token=must-not-be-logged",
			RequestID:        "request-must-not-be-logged",
		},
	)

	logged := output.String()
	for _, want := range []string{
		"msg=\"bff session refresh failed\"",
		"reason=refresh_failed",
		"error_kind=workos_api",
		"workos_status=401",
		"workos_code=invalid_client",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log = %q, want %q", logged, want)
		}
	}
	if strings.Contains(logged, "must-not-be-logged") {
		t.Fatalf("unsafe WorkOS error detail was logged: %q", logged)
	}
}
