//go:build sidecar_disruption && sidecar_pending

// §22.8's Postgres disruption row on T3 (TEST-M2-15; disruption_t3_test.go has the other rows and says what each
// shows): a request whose commit is cut by a Postgres restart. The request, POST /network/auth-keys, writes MG1's
// network_auth_key, which only the pending groups create, so the file builds only with sidecar_pending beside
// sidecar_disruption, as sidecar-t3.sh's nightly form runs it; without it the row fails at its first count of keys. It
// needs the private test stores and fails rather than skips without them.

package sidecart3

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// What a request is marked with, so the commit hook acts on its commit alone.
type disruptionRequestKey struct{}

// POST /network/auth-keys's transaction has its connection terminated at its commit, as a Postgres or PgBouncer restart
// mid-request does. The api answers retryable, §22.8's first half: the router maps the raised 57P01 to 503 with
// Retry-After: 1 and a fixed body that carries none of the cause (QD-759), as it answers a lock timeout. Nothing of
// the request took effect, and the same request sent again makes one key, so the retry has no double effect, its
// second half.
func TestT3PostgresRestartMidRequest(t *testing.T) {
	disruptionTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), testDisruptionTimeout)
		defer cancel()
		net := sidecartest.NewNet(ctx, nil)
		defer net.Close()
		api := NewT3Api(ctx, net, nil)
		networkId := server.NewId()
		userId := server.NewId()
		networkName := fmt.Sprintf("disrupt%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		root := "Bearer " + session.NewByJwt(networkId, userId, networkName, false, false).Testing_Sign()

		restart := NewPostgresRestart(nil)
		restarted := make(chan error, 1)
		server.SetPgCommitFaultForTest(t, func(ctx context.Context, commit func() error, rollback func()) error {
			if ctx.Value(disruptionRequestKey{}) == nil {
				return commit()
			}
			select {
			case restarted <- restart.Disrupt(context.WithoutCancel(ctx)):
			default:
			}
			return commit()
		})
		keyCount := func() int {
			count := 0
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_auth_key WHERE network_id = $1`, networkId).Scan(&count))
			})
			return count
		}
		before := keyCount()
		args := &model.CreateAuthKeyArgs{DurationSeconds: 600, MaxUses: 1}
		encodedArgs, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		// sent as api.Call sends it, and through the transport itself so the answer's headers are read
		request, err := http.NewRequestWithContext(context.WithValue(ctx, disruptionRequestKey{}, true), http.MethodPost, api.settings.ApiOrigin+"/network/auth-keys", bytes.NewReader(encodedArgs))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", root)
		request.Header.Set("Content-Type", "application/json")
		response, err := api.Transport().RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		status := response.StatusCode
		select {
		case err := <-restarted:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("the request committed nothing for the restart to cut: %d %s", status, answer)
		}
		if status == http.StatusOK {
			t.Fatalf("a commit on a terminated connection answered 200 %s", answer)
		}
		t.Logf("a commit cut by a Postgres restart answers %d %s", status, strings.TrimSpace(string(answer)))
		if retryAfter := response.Header.Get("Retry-After"); status != http.StatusServiceUnavailable || retryAfter != "1" {
			t.Errorf("the api answered %d with Retry-After %q, want the retryable 503 with 1", status, retryAfter)
		}
		if body := strings.TrimSpace(string(answer)); body != `{"error":"Temporarily unavailable. Retry shortly."}` {
			t.Errorf("the retryable answer's body is %q, which is not the fixed one", body)
		}
		if after := keyCount(); after != before {
			t.Errorf("the cut request left %d keys, from %d", after, before)
		}
		status, answer, err = api.Call(ctx, http.MethodPost, "/network/auth-keys", root, args)
		if err != nil || status != http.StatusOK {
			t.Fatalf("the request after the restart: %d %s %v", status, answer, err)
		}
		result := &model.CreateAuthKeyResult{}
		if err := json.Unmarshal(answer, result); err != nil || result.AuthKey == "" {
			t.Fatalf("the request after the restart answered %s", answer)
		}
		if after := keyCount(); after != before+1 {
			t.Errorf("the request after the restart left %d keys, from %d", after, before)
		}
	})
}
