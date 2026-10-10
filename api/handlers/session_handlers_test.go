package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func TestSessionOperationStatusRouteReportsPrepared202(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		network, user, operation := server.NewId(), server.NewId(), server.NewId()
		claims := session.NewByJwt(network, user, "session-status-http", false, false)
		actor, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,'session-status-http','password',true)`, user))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,'session-status-http',$2)`, network, user))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_session_operation(network_id,operation_id,action,fingerprint,actor,credential_epoch,status,quota_reserved,create_time,update_time,retain_until) VALUES($1,$2,'single','fixture',$3,$4,'prepared',false,$4,$4,$5)`, network, operation, actor, server.NowUtc(), server.NowUtc().Add(time.Hour)))
		})
		routes := router.NewRouter(ctx, []*router.Route{router.NewRoute(http.MethodGet, "/network/session-operations/([^/]+)", NetworkSessionOperation)})
		req := httptest.NewRequest(http.MethodGet, "/network/session-operations/"+operation.String(), nil)
		req.RemoteAddr = "192.0.2.1:1"
		req.Header.Set("Authorization", "Bearer "+claims.Testing_Sign())
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		var result session.SessionOperationResult
		if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.OperationId != operation || result.State != "prepared" || result.Status != "pending" {
			t.Fatal("prepared status route did not use202", w.Code, w.Body.String())
		}
	})
}

func TestSessionRevokeAuthenticationFailurePreservesOperationIdentity(t *testing.T) {
	for _, operation := range []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/network/revoke-session", RevokeNetworkSession}, {"/network/revoke-other-sessions", RevokeOtherNetworkSessions},
	} {
		for _, cancelled := range []bool{false, true} {
			id, target := server.NewId(), server.NewId()
			body := []byte(`{"operation_id":"` + id.String() + `","session_id":"` + target.String() + `"}`)
			ctx, cancel := context.WithCancel(t.Context())
			if cancelled {
				cancel()
			}
			req := httptest.NewRequest(http.MethodPost, operation.path, bytes.NewReader(body)).WithContext(ctx)
			req.RemoteAddr = "192.0.2.1:1"
			req.Header.Set("Authorization", "Bearer synthetically-invalid")
			w := httptest.NewRecorder()
			operation.handler(w, req)
			cancel()
			if cancelled {
				var result struct {
					OperationId server.Id `json:"operation_id"`
					Status      string    `json:"status"`
				}
				if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.OperationId != id || result.Status != "unknown" {
					t.Fatal("authentication outage lost operation identity", operation.path, w.Code, w.Body.String())
				}
			} else if w.Code != http.StatusUnauthorized {
				t.Fatal("definite malformed credential was not401", operation.path, w.Code)
			}
		}
	}
}

func TestSessionOperationFormatterUses202OnlyForPreparedPending(t *testing.T) {
	for _, item := range []struct {
		state, status string
		accepted      bool
	}{
		{"prepared", "pending", true}, {"enforced", "revoked", false}, {"complete", "revoked", false},
		{"cancelled", "cancelled", false}, {"failed", "failed", false}, {"unknown", "unknown", false}, {"enforced", "pending", false},
	} {
		t.Run(item.state+"/"+item.status, func(t *testing.T) {
			w := httptest.NewRecorder()
			id := server.NewId()
			result := &session.SessionOperationResult{OperationId: id, State: item.state, Status: item.status}
			if handled := sessionOperationFormatter(w)(nil, result); handled != item.accepted {
				t.Fatal("incorrect formatter decision", handled)
			}
			if item.accepted {
				var decoded session.SessionOperationResult
				if w.Code != http.StatusAccepted || json.Unmarshal(w.Body.Bytes(), &decoded) != nil || decoded.OperationId != id || decoded.State != "prepared" || w.Header().Get("Retry-After") != "1" {
					t.Fatal("pending receipt did not have202 envelope", w.Code, w.Body.String())
				}
			} else if w.Code != http.StatusOK || w.Body.Len() != 0 {
				t.Fatal("formatter intercepted authoritative result", w.Code)
			}
		})
	}
}
