package session

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
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
func (self *ClientSession) Auth(req *http.Request) (returnErr error) {
	authCtx, cancel := context.WithTimeout(self.Ctx, AuthTimeout)
	defer cancel()
	method := requestAuthMethod(req)
	defer func() { recordSessionAuth(method, self.ByJwt, returnErr) }()
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
			self.ByJwt = nil
		}
	}()
	return self.authenticate(authCtx, req)
}

func authDependencyUnavailable(err error) bool {
	if errors.Is(err, server.DbContextDoneError) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	var connectError *pgconn.ConnectError
	return errors.As(err, &connectError)
}
