package controller

// The auth routes refuse a malformed user auth, an unknown account and an
// unknown reset code with a 400. Without a declared status the router answers
// a refusal with its fixed 500, and the apps that show the refusal's text
// would lose it.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// Requires the router's answer to err to be status with message.
func requireRefusalAnswer(t testing.TB, err error, status int, message string) {
	t.Helper()
	w := httptest.NewRecorder()
	if err == nil {
		t.Fatalf("want %d %q, got no error", status, message)
	}
	router.RaiseHttpError(err, w)
	if w.Code != status || w.Body.String() != message+"\n" {
		t.Fatalf("want %d %q, got %d %q for %q", status, message, w.Code, w.Body.String(), err)
	}
}

func TestAuthPasswordResetRefusesMalformedUserAuthAsBadRequest(t *testing.T) {
	clientSession := session.Testing_CreateClientSession(context.Background(), nil)
	defer clientSession.Cancel()
	_, err := authPasswordReset(AuthPasswordResetArgs{UserAuth: "not-an-email-or-phone"}, clientSession, nil, nil)
	requireRefusalAnswer(t, err, http.StatusBadRequest, "Invalid user auth.")
}

func TestAuthPasswordResetRefusesUnknownAccountAsBadRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		clientSession := session.Testing_CreateClientSession(tb.Context(), nil)
		defer clientSession.Cancel()
		_, err := AuthPasswordReset(AuthPasswordResetArgs{UserAuth: "unknown-reset@example.invalid"}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusBadRequest, "Invalid login.")
	})
}

func TestAuthVerifySendRefusesUnknownAccountAsBadRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		clientSession := session.Testing_CreateClientSession(tb.Context(), nil)
		defer clientSession.Cancel()
		_, err := AuthVerifySend(AuthVerifySendArgs{UserAuth: "unknown-verify@example.invalid"}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusBadRequest, "Invalid login.")
	})
}

func TestAuthPasswordSetRefusesUnknownResetCodeAsBadRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		clientSession := session.Testing_CreateClientSession(tb.Context(), nil)
		defer clientSession.Cancel()
		_, err := AuthPasswordSet(model.AuthPasswordSetArgs{ResetCode: "synthetic-unknown-reset-code", Password: "synthetic-password-not-a-secret"}, clientSession)
		requireRefusalAnswer(tb, err, http.StatusBadRequest, "Invalid login.")
	})
}
