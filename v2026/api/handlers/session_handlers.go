package handlers

import (
	"encoding/json"
	"errors"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
	"net/http"
)

func sessionOperationFormatter(w http.ResponseWriter) router.FormatFunction[*session.SessionOperationResult] {
	return func(_ *session.ClientSession, result *session.SessionOperationResult) bool {
		if result == nil || result.State != "prepared" || result.Status != "pending" {
			return false
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			router.RaiseHttpError(err, w)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(append(encoded, '\n'))
		return true
	}
}

func NetworkSessions(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(session.GetNetworkSessions, w, r)
}
func RevokeNetworkSession(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(func(args *session.RevokeSessionArgs, actor *session.ClientSession) (*session.SessionOperationResult, error) {
		if err := authenticateSessionOperation(actor, r, args.OperationId); err != nil {
			return nil, err
		}
		return session.RevokeNetworkSession(args, actor)
	}, w, r, sessionOperationFormatter(w))
}
func RevokeOtherNetworkSessions(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputNoAuth(func(args *session.RevokeOtherSessionsArgs, actor *session.ClientSession) (*session.SessionOperationResult, error) {
		if err := authenticateSessionOperation(actor, r, args.OperationId); err != nil {
			return nil, err
		}
		return session.RevokeOtherNetworkSessions(args, actor)
	}, w, r, sessionOperationFormatter(w))
}

// Authenticate inside the decoded operation boundary so a dependency failure
// before preparation still carries the caller's recovery identity.
func authenticateSessionOperation(actor *session.ClientSession, r *http.Request, operationId server.Id) error {
	if err := actor.Auth(r); err != nil {
		if errors.Is(err, session.ErrAuthUnavailable) || errors.Is(err, session.ErrSessionStoreUnavailable) {
			return &session.SessionOperationUnavailableError{OperationId: operationId}
		}
		if errors.Is(err, session.ErrSessionRevoked) {
			return &session.SessionError{Code: "session_revoked", Status: 401}
		}
		return &session.SessionError{Code: "credential_rejected", Status: 401}
	}
	return nil
}
func NetworkSessionOperation(w http.ResponseWriter, r *http.Request) {
	values := router.GetPathValues(r)
	router.WrapNoAuth(func(clientSession *session.ClientSession) (*session.SessionOperationResult, error) {
		if len(values) != 1 {
			return nil, &session.SessionError{Code: "invalid_request", Status: 400}
		}
		id, err := server.ParseId(values[0])
		if err != nil {
			return nil, &session.SessionError{Code: "invalid_request", Status: 400}
		}
		if err := authenticateSessionOperation(clientSession, r, id); err != nil {
			return nil, err
		}
		return session.GetSessionOperation(clientSession, id)
	}, w, r, sessionOperationFormatter(w))
}
