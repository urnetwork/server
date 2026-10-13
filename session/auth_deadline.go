package session

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Authentication is admission work, separate from the admitted request's
// serving and financial lifetime. Earlier caller deadlines remain effective.
const AuthTimeout = 30 * time.Second

// ErrAuthUnavailable distinguishes dependency failure from invalid credentials.
// It contains no token, identity, database detail or query text.
var ErrAuthUnavailable = errors.New("authentication temporarily unavailable")

// Auth validates against current account state under one admission deadline.
// It never replaces self.Ctx: successful controllers retain their original
// lifetime, including the existing durable financial handoff boundaries.
func (self *ClientSession) Auth(req *http.Request) error {
	_, err := self.AuthUnlessAnswered(req, nil)
	return err
}

// Auth, except that answer may reply to a client credential's request before
// its client state query (see BeforeStateAnswer). An answered request is not
// authenticated: ByJwt stays nil and no authentication attempt is recorded.
// Every refusal, deadline and dependency failure is reported as by Auth.
func (self *ClientSession) AuthUnlessAnswered(req *http.Request, answer BeforeStateAnswer) (answered bool, returnErr error) {
	authCtx, cancel := context.WithTimeout(self.Ctx, AuthTimeout)
	defer cancel()
	method := requestAuthMethod(req)
	defer func() {
		if !answered {
			recordSessionAuth(method, self.ByJwt, returnErr)
		}
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			err, ok := recovered.(error)
			if !ok || !authDependencyUnavailable(err) {
				panic(recovered)
			}
			returnErr = ErrAuthUnavailable
		}
		if authCtx.Err() != nil || authDependencyUnavailable(returnErr) {
			returnErr = ErrAuthUnavailable
		}
		if returnErr != nil {
			answered = false
			self.ByJwt = nil
			self.ApiKeyAuthenticated = false
		}
	}()
	return self.authenticate(authCtx, req, answer)
}

func authDependencyUnavailable(err error) bool {
	if errors.Is(err, ErrSessionStoreUnavailable) || errors.Is(err, ErrAuthUnavailable) {
		return true
	}
	if errors.Is(err, server.DbContextDoneError) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	var connectError *pgconn.ConnectError
	return errors.As(err, &connectError)
}
