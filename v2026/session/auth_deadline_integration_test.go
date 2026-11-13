package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"github.com/urnetwork/server/v2026/session"
)

func deadlineAuthCredential(t testing.TB, ctx context.Context) string {
	t.Helper()
	network, user, device, client := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, network, "auth-deadline-control", user)
	model.Testing_CreateDevice(ctx, network, device, client, "auth-deadline-device", "test")
	return session.NewByJwt(network, user, "auth-deadline-control", false, false).Client(device, client).Testing_Sign()
}

func deadlineAuthRequest(ctx context.Context, credential string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "http://localhost/connect/control", bytes.NewBufferString("{}"))
	request.RemoteAddr = "127.0.0.1:1"
	request.Header.Set("Authorization", "Bearer "+credential)
	return request.WithContext(ctx)
}

// The only deadline ending this blocked state query is the production Auth
// deadline; the original request is still alive when the normal 503 returns.
func TestAuthDeadlineBlockedLiveStateReturns503AndNoAuthProgresses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		credential := deadlineAuthCredential(t, ctx)
		connection, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer connection.Release()
		server.RaisePgResult(connection.Exec(ctx, `SET default_transaction_read_only=off`))
		held, err := connection.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `LOCK TABLE network_user IN ACCESS EXCLUSIVE MODE`))
		var blocker int32
		server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blocker))
		response := httptest.NewRecorder()
		done := make(chan any, 1)
		called := false
		begin := time.Now()
		go func() {
			defer func() { done <- recover() }()
			request, finish := router.ObserveConnectControl(deadlineAuthRequest(ctx, credential))
			defer finish()
			router.WrapWithInputRequireClient(func(_ map[string]any, s *session.ClientSession) (map[string]bool, error) {
				called = true
				return map[string]bool{"ok": true}, nil
			}, response, request)
		}()
		for {
			var blocked bool
			server.Raise(held.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted AND $1::int=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked))
			if blocked {
				break
			}
			select {
			case <-done:
				t.Fatal("authentication exited before the real state lock barrier")
			default:
			}
			if ctx.Err() != nil {
				t.Fatal("state query did not reach real lock barrier")
			}
			runtime.Gosched()
		}
		publicResponse := httptest.NewRecorder()
		publicCalled := false
		router.WrapNoAuth(func(s *session.ClientSession) (map[string]bool, error) {
			publicCalled = true
			if s.ByJwt != nil {
				t.Fatal("public wrapper attempted authentication")
			}
			return map[string]bool{"ok": true}, nil
		}, publicResponse, deadlineAuthRequest(ctx, credential))
		if !publicCalled || publicResponse.Code != http.StatusOK {
			t.Fatal("public wrapper was blocked by the authentication dependency")
		}
		select {
		case panicValue := <-done:
			if panicValue != nil {
				t.Fatal("authentication timeout escaped the normal response boundary")
			}
		case <-time.After(session.AuthTimeout + 3*time.Second - time.Since(begin)):
			t.Fatal("authentication did not join its scoped deadline")
		}
		if called || ctx.Err() != nil || response.Code != http.StatusServiceUnavailable {
			t.Fatal("deadline admitted controller work, canceled original caller, or did not return503")
		}
		var body map[string]string
		if response.Header().Get("Content-Type") != "application/json" || json.Unmarshal(response.Body.Bytes(), &body) != nil || body["error"] != "Authentication temporarily unavailable." {
			t.Fatal("authentication dependency failure did not produce fixed503 JSON")
		}
	})
}

func TestAuthDeadlineCallerCancellationJoinsWithout401(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		credential := deadlineAuthCredential(t, ctx)
		caller, stop := context.WithCancel(ctx)
		stop()
		s := session.NewLocalClientSession(caller, "127.0.0.1:1", nil)
		defer s.Cancel()
		begin := time.Now()
		if err := s.Auth(deadlineAuthRequest(caller, credential)); !errors.Is(err, session.ErrAuthUnavailable) {
			t.Fatal("canceled dependency was accepted or classified as invalid credentials")
		}
		if time.Since(begin) > time.Second || s.ByJwt != nil {
			t.Fatal("canceled authentication did not join without claims")
		}
		response := httptest.NewRecorder()
		router.WrapRequireClient(func(s *session.ClientSession) (bool, error) {
			t.Fatal("canceled authentication reached controller")
			return false, nil
		}, response, deadlineAuthRequest(caller, credential))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatal("canceled authentication wrapper did not return503")
		}
	})
}

// The auth child has been canceled on return, but admitted SQL and the existing
// financial commit boundary retain the original session context.
func TestAuthDeadlineHealthyHandoffKeepsFinancialCommitContext(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		credential := deadlineAuthCredential(t, ctx)
		s := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
		defer s.Cancel()
		original := s.Ctx
		if err := s.Auth(deadlineAuthRequest(ctx, credential)); err != nil {
			t.Fatal("healthy fixture credential was refused")
		}
		if s.Ctx != original || s.Ctx.Err() != nil {
			t.Fatal("admitted session retained or was canceled by authentication child")
		}
		server.Db(s.Ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(s.Ctx, `CREATE TABLE auth_deadline_commit_control (id integer PRIMARY KEY)`))
		}, server.OptReadWrite())
		server.Tx(s.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(s.Ctx, `INSERT INTO auth_deadline_commit_control VALUES (1)`))
			s.Cancel()
		})
		var committed bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM auth_deadline_commit_control WHERE id=1)`).Scan(&committed))
		})
		if !committed {
			t.Fatal("successful financial body was not committed after caller cancellation")
		}
	})
}
