package controller

// sn_ctf_api_not_started_db_test.go — GET /sn/ctf/client once ctf.yml turns
// capture the flag on, before its pool starts, over the real routes, source,
// records and credentials: vault2 answers for the CTF pool, st.yml has v2
// addresses and no ctf block, and the chain is in epoch 6.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// snCtfTestCtfYml is a ctf.yml that turns every block on before the pool
// starts: presence, the census from epoch 7 (the shadow epoch before the first
// CTF epoch, 8) with no network declared the operator's own, and the routes.
const snCtfTestCtfYml = `presence:
  enabled: true
  retention_epochs: 4
census:
  enabled: true
  first_epoch: 7
  own_network_ids: []
  object_retention_epochs: 8
  member_retention_epochs: 60
api:
  enabled: true
  page_url: https://ur.xyz/ctf
`

// snCtfNotStartedFixture serves the routes as a server does once ctf.yml turns
// capture the flag on: the real source over st.yml's v2 identity without a ctf
// block, vault2 answering for the CTF pool (unregistered at first), and the
// chain at epoch 6's start, behind a load balancer that answers for no origin.
type snCtfNotStartedFixture struct {
	cfg   *StConfig
	chain *fakeLedgerV2Chain
	// epoch 6's start
	start     time.Time
	popCtfYml func()
}

func newSnCtfNotStartedFixture(t testing.TB) *snCtfNotStartedFixture {
	f := &snCtfNotStartedFixture{cfg: ledgerV2TestConfig(), start: time.Date(2026, 11, 13, 18, 27, 36, 0, time.UTC)}
	f.chain = newFakeLedgerV2Chain(&[]string{})
	f.chain.registered, f.chain.firstEpoch = false, 0
	SetStConfig(f.cfg)
	SetStLedgerV2Client(f.chain)
	SetStClient(newStubStClient(&StEpochState{Epoch: 6, EpochStartBlock: 9_499_228, TEpochBlocks: 50_400, HeadBlock: 9_499_228, HeadBlockTime: f.start}))
	corsOrigins := snOperatorCorsOrigins
	snOperatorCorsOrigins = func() ([]string, bool) { return nil, true }
	f.popCtfYml = server.Vault.PushSimpleResource("ctf.yml", []byte(snCtfTestCtfYml))
	f.reset()
	t.Cleanup(func() {
		f.popCtfYml()
		SetStConfig(nil)
		SetStLedgerV2Client(nil)
		SetStClient(nil)
		snOperatorCorsOrigins = corsOrigins
		f.reset()
	})
	return f
}

// reset forgets what the routes have read: ctf.yml's api block, the pool's
// registration, block times and the view.
func (self *snCtfNotStartedFixture) reset() {
	resetSnCtfApiConfigCache()
	snCtfPoolRegistrations.Clear()
	snCtfBlockTimes.lock.Lock()
	snCtfBlockTimes.times = nil
	snCtfBlockTimes.lock.Unlock()
	snCtfResetViewCache()
}

// Once ctf.yml turns capture the flag on, GET /sn/ctf/client answers every
// credential that authenticates, before the CTF pool starts, as it answers
// before the pool's first root: no epochs and no wins, never 503, since
// nothing has been drawn or won. That holds while vault2 has not registered the
// pool, and once it has, until the pool's first epoch begins, while st.yml
// names no carry yet. The state routes, whose answers name the pool's first
// epoch and its carry, stay unavailable until then; once the first epoch has
// begun without a carry, and with ctf.yml off, the client route is unavailable
// too, as before.
func TestDbSnCtfClientBeforeThePoolStarts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, userId, clientId, deviceId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "ctfnotstarted", userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "ctf not started", "test")
		networkSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId})
		apiKey, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: "ctf"}, networkSession)
		if err != nil || apiKey.Error != nil || apiKey.ApiKey == "" {
			t.Fatalf("api key: %+v %v", apiKey, err)
		}
		clientToken := session.NewByJwt(networkId, userId, "ctfnotstarted", false, false).Client(deviceId, clientId).Testing_Sign()
		networkToken := session.NewByJwt(networkId, userId, "ctfnotstarted", false, false).Testing_Sign()
		tokens := []string{clientToken, networkToken, apiKey.ApiKey}

		f := newSnCtfNotStartedFixture(t)
		ask := func(token string) *httptest.ResponseRecorder {
			return serveSnCtf(SnCtfClient, "/sn/ctf/client", "Authorization", "Bearer "+token)
		}
		// every credential: no epochs and no wins, as of this epoch's start
		requireNoWinsYet := func(when string) {
			t.Helper()
			for i, token := range tokens {
				response := ask(token)
				if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "private, no-cache" || response.Header().Get("ETag") == "" {
					t.Fatalf("%s, credential %d: %d %s", when, i, response.Code, response.Body.String())
				}
				var answer any
				if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
					t.Fatalf("%s, credential %d: %q: %v", when, i, response.Body.String(), err)
				}
				want := map[string]any{"as_of": "2026-11-13T18:27:36Z", "unit": snCtfTestJson(t, snCtfUnit()), "client_id": nil, "epochs": []any{}, "wins": []any{}}
				if token == clientToken {
					want["client_id"] = clientId.String()
				}
				requireSnCtfEqual(t, when, answer, want)
			}
		}
		requireStateUnavailable := func() {
			t.Helper()
			requireSnProblem(t, serveSnCtf(SnCtf, "/sn/ctf"), http.StatusServiceUnavailable, SnProblemUnavailable)
			requireSnProblem(t, serveSnCtf(SnCtfTop, "/sn/ctf/top"), http.StatusServiceUnavailable, SnProblemUnavailable)
			requireSnProblem(t, serveSnCtf(func(w http.ResponseWriter, r *http.Request) { SnCtfCountry(w, r, "is") }, "/sn/ctf/country/is"), http.StatusServiceUnavailable, SnProblemUnavailable)
			requireSnProblem(t, serveSnCtf(func(w http.ResponseWriter, r *http.Request) { SnCtfEpoch(w, r, "8") }, "/sn/ctf/epoch/8"), http.StatusServiceUnavailable, SnProblemUnavailable)
		}
		requireClientUnavailable := func() {
			t.Helper()
			for _, token := range tokens {
				requireSnProblem(t, ask(token), http.StatusServiceUnavailable, SnProblemUnavailable)
			}
		}

		// vault2 has not registered the CTF pool yet
		requireNoWinsYet("an unregistered pool")
		requireStateUnavailable()

		// registered from epoch 8, in epoch 6, before st.yml names a carry
		f.chain.registered, f.chain.firstEpoch = true, 8
		f.reset()
		requireNoWinsYet("a pool registered from epoch 8")
		requireStateUnavailable()

		// the pool's first epoch has begun without a carry: the driver is not
		// configured, so the state cannot be read
		f.chain.firstEpoch = 6
		f.reset()
		requireClientUnavailable()

		// ctf.yml off: unavailable, as before
		f.chain.firstEpoch = 8
		f.popCtfYml()
		f.reset()
		requireClientUnavailable()
		requireStateUnavailable()
	})
}
