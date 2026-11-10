//go:build sidecar_pending

package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// POST /network/enroll (TEAM-M1-04; §7.8, §22.7's auth keys), which needs MG0 and MG1.

type enrollFixture struct {
	ctx       context.Context
	networkId server.Id
	userId    server.Id
	root      *session.ClientSession
}

func newEnrollFixture(t testing.TB, preview bool) *enrollFixture {
	t.Helper()
	ctx := t.Context()
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "enroll-"+networkId.String(), userId)
	root := session.NewLocalClientSession(ctx, "192.0.2.1:1", session.NewByJwt(networkId, userId, "enroll-"+networkId.String(), false, false))
	t.Cleanup(root.Cancel)
	if preview {
		t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId)))
	}
	return &enrollFixture{ctx: ctx, networkId: networkId, userId: userId, root: root}
}

// A new auth key's bearer and id.
func (self *enrollFixture) authKey(t testing.TB, args *model.CreateAuthKeyArgs) (bearer string, authKeyId server.Id) {
	t.Helper()
	if args.DurationSeconds == 0 {
		args.DurationSeconds = 3600
	}
	if args.MaxUses == 0 {
		args.MaxUses = 10
	}
	result, err := model.CreateAuthKey(args, self.root)
	if err != nil || result.Error != nil || result.AuthKeyId == nil {
		t.Fatalf("create auth key: %v %+v", err, result)
	}
	return result.AuthKey, *result.AuthKeyId
}

func newEnrollClientKey(t testing.TB) ed25519.PublicKey {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

func (self *enrollFixture) enroll(args *model.NetworkEnrollArgs) (*model.NetworkEnrollResult, error) {
	clientSession := session.NewLocalClientSession(self.ctx, "192.0.2.9:1", nil)
	defer clientSession.Cancel()
	return NetworkEnroll(args, clientSession)
}

func requireEnrollRefusal(t testing.TB, err error, status int, code string) {
	t.Helper()
	refusal := &model.NetworkEnrollRefusal{}
	if !errors.As(err, &refusal) || refusal.Status != status || refusal.Code != code {
		t.Fatalf("refusal = %v, want %d %s", err, status, code)
	}
}

type enrollOwnerRow struct {
	ownerUserId      *server.Id
	memberId         *server.Id
	sponsorUserId    *server.Id
	authKeyId        *server.Id
	tagged           bool
	ephemeral        bool
	adminRefused     bool
	strictExp        bool
	declaresNoEgress bool
	aclEnforcing     bool
	clientPublicKey  []byte
}

func readEnrollOwnerRow(t testing.TB, ctx context.Context, clientId server.Id) (row *enrollOwnerRow) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT owner_user_id, member_id, sponsor_user_id, auth_key_id, tagged, ephemeral, admin_refused,
					strict_exp, declares_no_egress, acl_enforcing, client_public_key
				FROM network_client_owner
				WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				row = &enrollOwnerRow{}
				server.Raise(result.Scan(&row.ownerUserId, &row.memberId, &row.sponsorUserId, &row.authKeyId, &row.tagged,
					&row.ephemeral, &row.adminRefused, &row.strictExp, &row.declaresNoEgress, &row.aclEnforcing, &row.clientPublicKey))
			}
		})
	})
	return
}

func readAuthKeyUseCount(t testing.TB, ctx context.Context, authKeyId server.Id) (useCount int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT use_count FROM network_auth_key WHERE auth_key_id = $1`, authKeyId).Scan(&useCount))
	})
	return
}

// An untagged key's enrollment: the reply confirms admin_refused, whatever the request said, the token is
// a 7-day sidecar token carrying the declaration, and the owner row is the key's creator's, with the key,
// its marks and the client's key. A child of the enrolled client inherits every mark.
func TestEnrollMintsTheKeysCreatorsSidecarClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, true)
		bearer, authKeyId := self.authKey(t, &model.CreateAuthKeyArgs{})
		clientPublicKey := newEnrollClientKey(t)

		result, err := self.enroll(&model.NetworkEnrollArgs{
			AuthKey:          bearer,
			ClientPublicKey:  clientPublicKey,
			DeviceSpec:       "test sidecar",
			DeclaresNoEgress: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, result.AdminRefused, true)
		connect.AssertEqual(t, result.ApprovalState, model.NetworkEnrollApprovalStatePending)
		connect.AssertEqual(t, result.Close, "")
		connect.AssertEqual(t, readAuthKeyUseCount(t, self.ctx, authKeyId), 1)

		byJwt, err := session.ParseByJwt(self.ctx, result.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, byJwt.StrictExp, true)
		connect.AssertEqual(t, byJwt.DeclaresNoEgress, true)
		connect.AssertEqual(t, byJwt.NetworkId, self.networkId)
		connect.AssertEqual(t, byJwt.UserId, self.userId)
		connect.AssertEqual(t, *byJwt.ClientId, result.ClientId)
		if d := byJwt.ExpiresAt.Time.Sub(server.NowUtc().Add(7 * 24 * time.Hour)); d < -time.Minute || time.Minute < d {
			t.Fatalf("exp = %s", byJwt.ExpiresAt.Time)
		}

		row := readEnrollOwnerRow(t, self.ctx, result.ClientId)
		if row == nil {
			t.Fatal("no owner row")
		}
		connect.AssertEqual(t, *row.ownerUserId, self.userId)
		connect.AssertEqual(t, *row.memberId, self.userId)
		connect.AssertEqual(t, row.sponsorUserId == nil, true)
		connect.AssertEqual(t, *row.authKeyId, authKeyId)
		connect.AssertEqual(t, row.tagged || row.ephemeral || row.aclEnforcing, false)
		connect.AssertEqual(t, row.adminRefused && row.strictExp && row.declaresNoEgress, true)
		connect.AssertEqual(t, bytes.Equal(row.clientPublicKey, clientPublicKey), true)
		connect.AssertEqual(t, model.ClientRefusesAdmin(self.ctx, self.networkId, result.ClientId), true)

		proofKey, err := model.ClientProofPublicKey(self.ctx, result.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, bytes.Equal(proofKey, clientPublicKey), true)

		enrolledSession := session.NewLocalClientSession(self.ctx, "192.0.2.9:1", byJwt)
		defer enrolledSession.Cancel()
		child, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{
			SourceClientId: &result.ClientId,
			Description:    "window",
			DeviceSpec:     "test",
		}, enrolledSession)
		if err != nil || child.Error != nil {
			t.Fatalf("child: %v %+v", err, child)
		}
		connect.AssertEqual(t, child.AdminRefused && child.StrictExp && child.DeclaresNoEgress, true)
		childJwt, err := session.ParseByJwt(self.ctx, *child.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, childJwt.DeclaresNoEgress, true)
		childRow := readEnrollOwnerRow(t, self.ctx, *child.ClientId)
		connect.AssertEqual(t, childRow != nil && childRow.adminRefused && childRow.strictExp && childRow.declaresNoEgress, true)
	})
}

// A tagged key's client has no owner and the key's creator sponsors it; before the network's base 1 a
// tagged or ephemeral key answers no_genesis, spending nothing, and with no log at hand 503.
func TestEnrollTaggedKeyWaitsForTheGenesis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, true)
		bearer, authKeyId := self.authKey(t, &model.CreateAuthKeyArgs{Tags: []string{"0123456789abcdef0123456789abcdef"}})
		args := &model.NetworkEnrollArgs{AuthKey: bearer, ClientPublicKey: newEnrollClientKey(t), DeviceSpec: "test"}

		_, err := self.enroll(args)
		requireEnrollRefusal(t, err, http.StatusServiceUnavailable, model.NetworkEnrollCodeUnavailable)
		hasGenesis := false
		t.Cleanup(model.Testing_SetNetworkEnrollHasGenesis(func(ctx context.Context, networkId server.Id) (bool, error) {
			return hasGenesis, nil
		}))
		_, err = self.enroll(args)
		requireEnrollRefusal(t, err, http.StatusForbidden, model.NetworkEnrollCodeNoGenesis)
		connect.AssertEqual(t, readAuthKeyUseCount(t, self.ctx, authKeyId), 0)

		hasGenesis = true
		result, err := self.enroll(args)
		if err != nil {
			t.Fatal(err)
		}
		row := readEnrollOwnerRow(t, self.ctx, result.ClientId)
		connect.AssertEqual(t, row.tagged, true)
		connect.AssertEqual(t, row.ownerUserId == nil && row.memberId == nil, true)
		connect.AssertEqual(t, *row.sponsorUserId, self.userId)
		tagged, err := model.ClientTaggedOrEphemeral(self.ctx, result.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, tagged, true)
	})
}

// An ephemeral key's enrollment returns its close capability once, and api keeps only its SHA-256; the
// capability closes the client.
func TestEnrollEphemeralReturnsItsCloseCapabilityOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, true)
		t.Cleanup(model.Testing_SetNetworkEnrollHasGenesis(func(ctx context.Context, networkId server.Id) (bool, error) {
			return true, nil
		}))
		bearer, _ := self.authKey(t, &model.CreateAuthKeyArgs{Ephemeral: true})
		result, err := self.enroll(&model.NetworkEnrollArgs{AuthKey: bearer, ClientPublicKey: newEnrollClientKey(t), DeviceSpec: "page"})
		if err != nil {
			t.Fatal(err)
		}
		capabilityBytes, err := base64.RawURLEncoding.DecodeString(result.Close)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(capabilityBytes), 16)
		var stored []byte
		server.Db(self.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(self.ctx, `SELECT close_sha256 FROM network_client_lease WHERE client_id = $1`, result.ClientId).Scan(&stored))
		})
		sum := sha256.Sum256(capabilityBytes)
		connect.AssertEqual(t, bytes.Equal(stored, sum[:]), true)
		connect.AssertEqual(t, readEnrollOwnerRow(t, self.ctx, result.ClientId).ephemeral, true)

		// the client's own reissue returns no capability and cannot ask to be ephemeral
		byJwt, err := session.ParseByJwt(self.ctx, result.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		enrolledSession := session.NewLocalClientSession(self.ctx, "192.0.2.9:1", byJwt)
		defer enrolledSession.Cancel()
		reissue, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{ClientId: &result.ClientId, DeviceSpec: "page"}, enrolledSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reissue.Error == nil && reissue.Close == "", true)
		again, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{ClientId: &result.ClientId, DeviceSpec: "page", Ephemeral: true}, enrolledSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.Error != nil, true)

		reaped, err := model.CloseEphemeralClient(self.ctx, result.Close)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reaped, true)
	})
}

// Two enrollments race for a single-use key, both held at the mint's barrier before the spend: one mints
// and the other answers auth_key_invalid, and the key is spent once.
func TestEnrollSingleUseRace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, true)
		bearer, authKeyId := self.authKey(t, &model.CreateAuthKeyArgs{MaxUses: 1})

		arrived := make(chan struct{}, 2)
		release := make(chan struct{})
		model.Testing_EnrollBeforeConsume = func() {
			arrived <- struct{}{}
			<-release
		}
		t.Cleanup(func() { model.Testing_EnrollBeforeConsume = nil })

		type outcome struct {
			result *model.NetworkEnrollResult
			err    error
		}
		outcomes := make(chan outcome, 2)
		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := self.enroll(&model.NetworkEnrollArgs{AuthKey: bearer, ClientPublicKey: newEnrollClientKey(t), DeviceSpec: "test"})
				outcomes <- outcome{result: result, err: err}
			}()
		}
		<-arrived
		<-arrived
		close(release)
		wg.Wait()
		close(outcomes)

		mintedCount := 0
		for outcome := range outcomes {
			if outcome.err == nil {
				mintedCount += 1
				continue
			}
			requireEnrollRefusal(t, outcome.err, http.StatusUnauthorized, model.NetworkEnrollCodeAuthKeyInvalid)
		}
		connect.AssertEqual(t, mintedCount, 1)
		connect.AssertEqual(t, readAuthKeyUseCount(t, self.ctx, authKeyId), 1)
	})
}

// A network outside preview_networks refuses the enrollment with not_preview and spends nothing; an API
// key, a malformed key, a missing client key and an admission are refused before anything is read.
func TestEnrollRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, false)
		bearer, authKeyId := self.authKey(t, &model.CreateAuthKeyArgs{})
		clientPublicKey := newEnrollClientKey(t)

		_, err := self.enroll(&model.NetworkEnrollArgs{AuthKey: bearer, ClientPublicKey: clientPublicKey, DeviceSpec: "test", StrictExp: true, AdminRefused: true})
		requireEnrollRefusal(t, err, http.StatusForbidden, model.NetworkEnrollCodeNotPreview)
		connect.AssertEqual(t, readAuthKeyUseCount(t, self.ctx, authKeyId), 0)

		for _, refused := range []*model.NetworkEnrollArgs{
			{AuthKey: "urn_synthetic-api-key", ClientPublicKey: clientPublicKey, DeviceSpec: "test"},
			{AuthKey: bearer, ClientPublicKey: clientPublicKey[:31], DeviceSpec: "test"},
			{AuthKey: bearer, ClientPublicKey: clientPublicKey, DeviceSpec: ""},
			{AuthKey: bearer, ClientPublicKey: clientPublicKey, DeviceSpec: "test", Admission: json.RawMessage(`{"statement":"x"}`)},
		} {
			_, err := self.enroll(refused)
			requireEnrollRefusal(t, err, http.StatusBadRequest, model.NetworkEnrollCodeBadRequest)
		}
		for _, invalid := range []string{"ur-authkey-not-a-key", bearer + "x", strings.Replace(bearer, "ur-authkey-", "", 1)[:20]} {
			_, err := self.enroll(&model.NetworkEnrollArgs{AuthKey: invalid, ClientPublicKey: clientPublicKey, DeviceSpec: "test"})
			requireEnrollRefusal(t, err, http.StatusUnauthorized, model.NetworkEnrollCodeAuthKeyInvalid)
		}
		connect.AssertEqual(t, readAuthKeyUseCount(t, self.ctx, authKeyId), 0)
	})
}

// §22.7's auth keys: an auth_key holding its # bundle is refused through the route with 400, and neither
// the answer nor the process's log, at the router's most verbose level, holds any of the key.
func TestEnrollRefusesABundleUnlogged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, true)
		bearer, _ := self.authKey(t, &model.CreateAuthKeyArgs{})
		secretPart := bearer[strings.LastIndex(bearer, "-")+1:]
		bundle := "synthetic-bundle-marker-0b9e"
		body, err := json.Marshal(map[string]any{
			"auth_key":          bearer + "#" + bundle,
			"client_public_key": []byte(newEnrollClientKey(t)),
			"device_spec":       "test",
		})
		if err != nil {
			t.Fatal(err)
		}

		previousVerbosity := flag.Lookup("v").Value.String()
		flag.Set("v", "2")
		defer flag.Set("v", previousVerbosity)
		read, write, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		realStderr := os.Stderr
		os.Stderr = write
		captured := make(chan []byte, 1)
		go func() {
			logged, _ := io.ReadAll(read)
			captured <- logged
		}()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/network/enroll", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.WrapWithInputNoAuth(NetworkEnroll, recorder, request)
		// the capture's own proof that it reads what glog writes
		glog.Infof("[test]enroll capture canary\n")
		glog.Flush()
		os.Stderr = realStderr
		write.Close()
		logged := string(<-captured)

		if !strings.Contains(logged, "enroll capture canary") {
			t.Fatal("the capture read nothing glog wrote")
		}
		connect.AssertEqual(t, recorder.Code, http.StatusBadRequest)
		answer := recorder.Body.String()
		if !strings.Contains(answer, model.NetworkEnrollCodeBadRequest) {
			t.Fatalf("answer %q", answer)
		}
		for _, part := range []string{bundle, secretPart, bearer} {
			if strings.Contains(answer, part) || strings.Contains(logged, part) {
				t.Fatalf("the key reached the answer or the log")
			}
		}
	})
}

// The limits count each enrollment against its key: past the key's limit the route answers 429
// rate_limited with a retry hint, while another key enrolls.
func TestEnrollIsLimitedPerKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, true)
		bearer, _ := self.authKey(t, &model.CreateAuthKeyArgs{})
		other, _ := self.authKey(t, &model.CreateAuthKeyArgs{})
		settings := DefaultNetworkEnrollSettings()
		settings.KeyLimit = 2
		clientSession := session.NewLocalClientSession(self.ctx, "192.0.2.10:1", nil)
		defer clientSession.Cancel()
		for range 2 {
			if _, err := networkEnrollWithSettings(&model.NetworkEnrollArgs{AuthKey: bearer, ClientPublicKey: newEnrollClientKey(t), DeviceSpec: "test"}, clientSession, settings); err != nil {
				t.Fatal(err)
			}
		}
		_, err := networkEnrollWithSettings(&model.NetworkEnrollArgs{AuthKey: bearer, ClientPublicKey: newEnrollClientKey(t), DeviceSpec: "test"}, clientSession, settings)
		refusal := &model.NetworkEnrollRefusal{}
		if !errors.As(err, &refusal) || refusal.Status != http.StatusTooManyRequests || refusal.Code != "rate_limited" || refusal.RetryAfterSeconds() <= 0 {
			t.Fatalf("past the key's limit: %v", err)
		}
		if _, err := networkEnrollWithSettings(&model.NetworkEnrollArgs{AuthKey: other, ClientPublicKey: newEnrollClientKey(t), DeviceSpec: "test"}, clientSession, settings); err != nil {
			t.Fatalf("another key: %v", err)
		}
	})
}

// plan-TEAM-enroll-client-key: an enrolled client binds only its enrolled key, so a copied token cannot
// bind a key of its own and renew under it.
func TestEnrolledClientBindsOnlyItsKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, true)
		bearer, _ := self.authKey(t, &model.CreateAuthKeyArgs{})
		clientPublicKey := newEnrollClientKey(t)
		result, err := self.enroll(&model.NetworkEnrollArgs{AuthKey: bearer, ClientPublicKey: clientPublicKey, DeviceSpec: "test"})
		if err != nil {
			t.Fatal(err)
		}
		bind := func(key []byte, signed []byte) model.SidecarBindOutcome {
			outcome, _ := model.BindSidecarIdentity(self.ctx, &model.SidecarBindingRecord{
				NetworkId:         self.networkId,
				SidecarId:         server.NewId(),
				ClientId:          result.ClientId,
				Generation:        1,
				ClientPublicKey:   key,
				IdentityPublicKey: newEnrollClientKey(t),
				SignedBinding:     signed,
			}, server.NowUtc())
			return outcome
		}
		connect.AssertEqual(t, bind(newEnrollClientKey(t), []byte("synthetic binding a")), model.SidecarBindClientBound)
		connect.AssertEqual(t, bind(clientPublicKey, []byte("synthetic binding b")), model.SidecarBindAppended)
	})
}

// auth-client takes the marks and the ephemeral mint: acl_enforcing shows in GET /network/clients, an
// ephemeral device client returns its close capability, a child cannot be ephemeral, and a re-auth cannot
// add a mark.
func TestAuthClientMarksAndTheClientsList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, true)
		device, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "phone", DeviceSpec: "test", AdminRefused: true, AclEnforcing: true}, self.root)
		if err != nil || device.Error != nil {
			t.Fatalf("device: %v %+v", err, device)
		}
		connect.AssertEqual(t, device.AclEnforcing && device.AdminRefused && !device.StrictExp, true)
		legacy, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "laptop", DeviceSpec: "test"}, self.root)
		if err != nil || legacy.Error != nil {
			t.Fatalf("legacy: %v %+v", err, legacy)
		}
		clients, err := model.GetNetworkClients(self.root)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[server.Id]bool{}
		for _, client := range clients.Clients {
			seen[client.ClientId] = client.AclEnforcing
		}
		connect.AssertEqual(t, seen[*device.ClientId], true)
		_, listed := seen[*legacy.ClientId]
		connect.AssertEqual(t, listed, true)
		connect.AssertEqual(t, seen[*legacy.ClientId], false)

		page, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "page", DeviceSpec: "test", StrictExp: true, Ephemeral: true}, self.root)
		if err != nil || page.Error != nil {
			t.Fatalf("page: %v %+v", err, page)
		}
		connect.AssertEqual(t, len(page.Close), 22)
		connect.AssertEqual(t, readEnrollOwnerRow(t, self.ctx, *page.ClientId).ephemeral, true)

		child, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{SourceClientId: device.ClientId, Description: "w", DeviceSpec: "test", Ephemeral: true}, self.root)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, child.Error != nil, true)

		added, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{ClientId: legacy.ClientId, DeviceSpec: "test", DeclaresNoEgress: true}, self.root)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, added.Error != nil, true)
	})
}

// The poll's lease touch (TEAM-M1-10 wired into PROTO-M1-10's long-poll): an ephemeral client's first poll
// moves its lease's seen_time, and a second poll inside the marker writes nothing.
func TestSidecarSyncPollTouchesTheLease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newEnrollFixture(t, true)
		t.Cleanup(model.Testing_SetNetworkEnrollHasGenesis(func(ctx context.Context, networkId server.Id) (bool, error) {
			return true, nil
		}))
		bearer, _ := self.authKey(t, &model.CreateAuthKeyArgs{Ephemeral: true})
		result, err := self.enroll(&model.NetworkEnrollArgs{AuthKey: bearer, ClientPublicKey: newEnrollClientKey(t), DeviceSpec: "page"})
		if err != nil {
			t.Fatal(err)
		}
		old := server.NowUtc().Add(-20 * time.Minute)
		setSeen := func(seenTime time.Time) {
			server.Tx(self.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(self.ctx, `UPDATE network_client_lease SET seen_time = $2 WHERE client_id = $1`, result.ClientId, seenTime))
			})
		}
		seen := func() (seenTime time.Time) {
			server.Db(self.ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(self.ctx, `SELECT seen_time FROM network_client_lease WHERE client_id = $1`, result.ClientId).Scan(&seenTime))
			})
			return
		}
		setSeen(old)

		server.Testing_EnableKeyspaceNotifications(self.ctx)
		defer server.Testing_PushSidecarConfig(testSidecarSyncPushOn)()
		defer Testing_CloseSidecarSyncLongPoll()
		byJwt, err := session.ParseByJwt(self.ctx, result.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		clientSession := session.NewLocalClientSession(self.ctx, "192.0.2.9:1", byJwt)
		defer clientSession.Cancel()
		if _, err := SidecarSyncPoll(nil, clientSession); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, old.Add(10*time.Minute).Before(seen()), true)

		setSeen(old)
		if _, err := SidecarSyncPoll(nil, clientSession); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, seen().Equal(old), true)
	})
}
