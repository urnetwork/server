package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

type nullBodyTestInput struct {
	Value string `json:"value"`
}

// A JSON null decodes successfully into a typed pointer. Before the input
// wrapper rejected it, public no-auth handlers received a nil pointer and
// dereferenced it as a server panic.
func TestWrapWithInputNoAuthRejectsJSONNullWithoutCallingPointerImplementation(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/input", strings.NewReader(" \n null \t"))
	recorder := httptest.NewRecorder()
	called := false

	WrapWithInputNoAuth(
		func(input *nullBodyTestInput, clientSession *session.ClientSession) (*nullBodyTestInput, error) {
			called = true
			if input == nil {
				t.Fatal("the null body reached the implementation")
			}
			return input, nil
		},
		recorder,
		request,
	)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%q", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if called {
		t.Fatal("implementation was called for a JSON null body")
	}
}

func TestWrapWithInputNoAuthAcceptsConcretePointerInputAfterNullGuard(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/input", strings.NewReader(`{"value":"healthy"}`))
	recorder := httptest.NewRecorder()
	called := false

	WrapWithInputNoAuth(
		func(input *nullBodyTestInput, clientSession *session.ClientSession) (*nullBodyTestInput, error) {
			called = true
			if input == nil || input.Value != "healthy" {
				t.Fatalf("input = %#v, want concrete decoded input", input)
			}
			return input, nil
		},
		recorder,
		request,
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !called {
		t.Fatal("implementation was not called for a concrete JSON object")
	}
}
