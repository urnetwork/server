package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestSessionOperationUnavailableKeepsIdentityWithoutDependencyCause(t *testing.T) {
	id := server.NewId()
	err := fmt.Errorf("[operation]%w", errors.Join(&session.SessionOperationUnavailableError{OperationId: id}, errors.New("private-redis-command-and-secret")))
	w := httptest.NewRecorder()
	if !RaiseHttpError(err, w) || w.Code != http.StatusServiceUnavailable {
		t.Fatal("unknown operation became a credential rejection", w.Code)
	}
	var body struct {
		OperationId server.Id `json:"operation_id"`
		Status      string    `json:"status"`
		State       string    `json:"state"`
		Code        string    `json:"code"`
	}
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.OperationId != id || body.Status != "unknown" || body.State != "unknown" || body.Code != "session_operation_unavailable" {
		t.Fatal("unknown operation lost its recovery identity", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-") || w.Header().Get("Retry-After") != "1" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("unsafe or untyped operation envelope", w.Header(), w.Body.String())
	}
}

func TestSessionOperationKnownRejectionRemains401(t *testing.T) {
	w := httptest.NewRecorder()
	if !RaiseHttpError(&session.SessionError{Code: "session_revoked", Status: 401}, w) || w.Code != http.StatusUnauthorized {
		t.Fatal("known rejection did not stay401", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"code":"session_revoked"`) || strings.Contains(w.Body.String(), `"status":"unknown"`) {
		t.Fatal("known rejection used unknown operation envelope", w.Body.String())
	}
}
