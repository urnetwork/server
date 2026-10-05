package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

func TestAuthDeadlineEveryAuthenticatedWrapperPreserves503(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		for _, wrapper := range []string{"require_auth", "require_client", "input_auth", "input_client", "optional_auth"} {
			t.Run(fmt.Sprintf("%s/canceled=%v", wrapper, canceled), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if canceled {
					cancel()
				}
				request := httptest.NewRequest(http.MethodPost, "http://localhost/control", bytes.NewBufferString("{}"))
				request = request.WithContext(ctx)
				request.RemoteAddr = "127.0.0.1:1"
				request.Header.Set("Authorization", "Bearer synthetically-invalid")
				response := httptest.NewRecorder()
				called := false
				impl := func(*session.ClientSession) (bool, error) { called = true; return true, nil }
				inputImpl := func(map[string]any, *session.ClientSession) (bool, error) { called = true; return true, nil }
				switch wrapper {
				case "require_auth":
					WrapRequireAuth(impl, response, request)
				case "require_client":
					WrapRequireClient(impl, response, request)
				case "input_auth":
					WrapWithInputRequireAuth(inputImpl, response, request)
				case "input_client":
					WrapWithInputRequireClient(inputImpl, response, request)
				case "optional_auth":
					WrapWithInputOptionalAuth(inputImpl, response, request)
				}
				want := http.StatusUnauthorized
				if canceled {
					want = http.StatusServiceUnavailable
				}
				if called || response.Code != want {
					t.Fatalf("controller called=%v, status=%d; want false,%d", called, response.Code, want)
				}
				if canceled && response.Header().Get("Content-Type") != "application/json" {
					t.Fatal("dependency failure did not use JSON")
				}
			})
		}
	}
}

func TestAuthDeadlineErrorResponseDoesNotExposeDependencyCause(t *testing.T) {
	response := httptest.NewRecorder()
	err := fmt.Errorf("[controller]%w", errors.Join(session.ErrAuthUnavailable, errors.New("synthetic-private-query-or-credential")))
	if !RaiseHttpError(err, response) || response.Code != http.StatusServiceUnavailable {
		t.Fatal("authentication sentinel did not own its normal HTTP status")
	}
	if response.Body.String() != "{\"error\":\"Authentication temporarily unavailable.\"}\n" || strings.Contains(response.Body.String(), "synthetic-private") {
		t.Fatal("authentication response exposed a dependency cause")
	}
}
