package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

func TestJsonRequestBodyLimitAcceptsExactBoundary(t *testing.T) {
	prefix := `{"padding":"`
	suffix := `"}`
	jsonRequestMaxByteCount := DefaultRouterSettings().JsonRequestMaxByteCount
	if jsonRequestMaxByteCount != 16*1024*1024 {
		t.Fatalf("default JSON request cap = %d, want the public load balancer's 16 MiB", jsonRequestMaxByteCount)
	}
	paddingBytes := int(jsonRequestMaxByteCount) - len(prefix) - len(suffix)
	body := prefix + strings.Repeat("x", paddingBytes) + suffix
	if int64(len(body)) != jsonRequestMaxByteCount {
		t.Fatalf("fixture body length = %d, want %d", len(body), jsonRequestMaxByteCount)
	}

	request := httptest.NewRequest(http.MethodPost, "/input", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	called := false

	WrapWithInputNoAuth(
		func(input map[string]any, clientSession *session.ClientSession) (map[string]any, error) {
			called = true
			return map[string]any{}, nil
		},
		recorder,
		request,
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !called {
		t.Fatal("handler was not called for an exact-limit body")
	}
}

func TestJsonRequestBodyLimitRejectsKnownAndStreamingLengths(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/input",
			strings.NewReader(strings.Repeat("x", int(DefaultRouterSettings().JsonRequestMaxByteCount)+1)),
		)
		if unknownLength {
			request.ContentLength = -1
		}
		recorder := httptest.NewRecorder()
		called := false

		WrapWithInputNoAuth(
			func(input map[string]any, clientSession *session.ClientSession) (map[string]any, error) {
				called = true
				return map[string]any{}, nil
			},
			recorder,
			request,
		)

		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("unknown length=%t: status = %d, want %d", unknownLength, recorder.Code, http.StatusRequestEntityTooLarge)
		}
		if called {
			t.Errorf("unknown length=%t: handler was called for an oversized body", unknownLength)
		}
	}
}

// The cap is the installed settings' and not a fixed number: a smaller one
// refuses a body the default takes, and restoring the defaults takes it again.
func TestJsonRequestBodyLimitFollowsTheSettings(t *testing.T) {
	body := `{"padding":"` + strings.Repeat("x", 64) + `"}`
	serve := func() int {
		recorder := httptest.NewRecorder()
		WrapWithInputNoAuth(
			func(input map[string]any, clientSession *session.ClientSession) (map[string]any, error) {
				return map[string]any{}, nil
			},
			recorder,
			httptest.NewRequest(http.MethodPost, "/input", strings.NewReader(body)),
		)
		return recorder.Code
	}

	settings := DefaultRouterSettings()
	settings.JsonRequestMaxByteCount = int64(len(body)) - 1
	SetRouterSettings(settings)
	defer SetRouterSettings(nil)
	if code := serve(); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a body over the settings' cap answered %d, want %d", code, http.StatusRequestEntityTooLarge)
	}

	settings = DefaultRouterSettings()
	settings.JsonRequestMaxByteCount = int64(len(body))
	SetRouterSettings(settings)
	if code := serve(); code != http.StatusOK {
		t.Fatalf("a body at the settings' cap answered %d, want %d", code, http.StatusOK)
	}

	SetRouterSettings(nil)
	if code := serve(); code != http.StatusOK {
		t.Fatalf("a body under the default cap answered %d, want %d", code, http.StatusOK)
	}
}
