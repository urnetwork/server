package controller

// sn_ctf_api_client_db_test.go — GET /sn/ctf/client over the real records and
// real credentials: a client token answers for its client, a network's token
// or API key for every client the network has had and no other network's; and
// the claim states of the CTF pool's accounts, read from its mirrored roots,
// their published accounts, the record of each root's deltas and vault2's
// claimed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// snCtfClaimsTestSource is the records' source with claim states given, and
// the needs it was asked for kept.
type snCtfClaimsTestSource struct {
	snCtfSource
	claims map[[32]byte]*snCtfAccountClaims
	lock   sync.Mutex
	needs  [][]*snCtfClaimNeed
}

func (self *snCtfClaimsTestSource) AccountClaims(_ context.Context, _ *snCtfView, needs []*snCtfClaimNeed) map[[32]byte]*snCtfAccountClaims {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.needs = append(self.needs, needs)
	claims := map[[32]byte]*snCtfAccountClaims{}
	for _, need := range needs {
		if claim, ok := self.claims[need.Coldkey]; ok {
			claims[need.Coldkey] = claim
		}
	}
	return claims
}

// The wins of an answer as "epoch role country rank client claimed".
func snCtfClientTestWins(t testing.TB, response *httptest.ResponseRecorder) (*SnCtfClientResult, []string) {
	t.Helper()
	var result SnCtfClientResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("answer %q: %v", response.Body.String(), err)
	}
	wins := []string{}
	for _, win := range result.Wins {
		claimed := "null"
		if win.Claimed != nil {
			claimed = fmt.Sprint(*win.Claimed)
		}
		wins = append(wins, fmt.Sprintf("%d %s %s %d %s %s", win.Epoch, win.Role, win.Country, win.Rank, win.ClientId, claimed))
	}
	return &result, wins
}

// Every credential that authenticates is answered, never 401: a client token
// for its own client, its status and its wins; the network's token and its API
// key for every client the network has had, a removed one included, and not a
// client of another network, and still once the reaper has hard-deleted every
// client's rows, since each award keeps the network it was won for. Only no
// token, or one that does not authenticate, is 401. With capture the flag
// off, a network's token is 503, as a client's is.
func TestDbSnCtfClientAnswersEveryCredential(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		parse := func(text string) server.Id {
			id, err := server.ParseId(text)
			if err != nil {
				t.Fatal(err)
			}
			return id
		}
		// the example's winners: the provider in Iceland (rank 1 in 9, rank 2
		// in 8) and Germany's first and second extenders in 9 are network N's,
		// the second removed since; Iceland's second provider in 9 is M's
		iceland, extender, removed, other := parse("3f6c2a1e-8d4b-4c7a-9e21-5b0d7f3a9c64"), parse("6a0f3c9d-71e2-4b58-8c3a-e94d2b6f1057"),
			parse("c2d85b16-3e9a-47f0-b1c4-8a7e05d93f2b"), parse("9b17e4d2-05c3-4f8e-a6b9-2d4c8e1f7a30")
		networkN, userN, networkM, userM := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkN, "ctfclientn", userN)
		model.Testing_CreateNetwork(ctx, networkM, "ctfclientm", userM)
		devices := map[server.Id]server.Id{}
		for clientId, networkId := range map[server.Id]server.Id{iceland: networkN, extender: networkN, removed: networkN, other: networkM} {
			devices[clientId] = server.NewId()
			model.Testing_CreateDevice(ctx, networkId, devices[clientId], clientId, "ctf client", "test")
		}
		// the awards are written once the clients exist, as the driver writes
		// an epoch's awards after its census: each with its client's network
		f := newSnCtfDbFixture(t)
		networkSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkN, UserId: userN})
		if result, err := model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: removed}, networkSession); err != nil || result.Error != nil {
			t.Fatalf("remove: %+v %v", result, err)
		}
		apiKey, err := model.CreateApiKey(&model.CreateApiKeyArgs{Name: "ctf"}, networkSession)
		if err != nil || apiKey.Error != nil || apiKey.ApiKey == "" {
			t.Fatalf("api key: %+v %v", apiKey, err)
		}
		// an award of epoch 10, which no committed root covers yet, is not a win
		hash, block := [32]byte{0x10}, uint64(9_751_300)
		if !model.CreateCtfEpochResult(ctx, &model.CtfEpochResult{DeploymentKey: f.cfg.DeploymentKey(), NoId: f.cfg.NoId, Epoch: 10, LedgerEpoch: 10, LedgerHash: hash, CensusHash: &hash, CensusBlock: &block, Round: 1, Drawn: true, Awards: 1},
			[]*model.CtfAward{{Epoch: 10, Role: snCtfRoleProvider, CountryCode: "is", Rank: 1, Unit: [32]byte{0x10}, UnitFamily: 4, ClientId: iceland, Coldkey: [32]byte{0x10}, Slot: snCtfTestRao(1)}},
			[]*model.CtfPot{}, []*model.CtfPendingAward{}) {
			t.Fatal("epoch 10 result not created")
		}

		core, err := newSnCtfCoreSource()
		if err != nil {
			t.Fatal(err)
		}
		// the Iceland provider's coldkey is claimed through epoch 8's root
		// (root 1); Germany's extenders' have claimed nothing
		icelandColdkey := snCtfTestHex32(t, "0xff05d4de50cdc2b6d6aca7b376cfdb00f65c4243f6799c4eaca182e877faf619")
		source := &snCtfClaimsTestSource{snCtfSource: core, claims: map[[32]byte]*snCtfAccountClaims{
			icelandColdkey: {Claimed: snCtfTestRao(48_025_660), Cumulative: map[uint64]uint256.Int{1: snCtfTestRao(48_025_660)}},
		}}
		withSnCtfTestSource(t, source)

		ask := func(token string, headers ...string) *httptest.ResponseRecorder {
			if token != "" {
				headers = append(headers, "Authorization", "Bearer "+token)
			}
			return serveSnCtf(SnCtfClient, "/sn/ctf/client", headers...)
		}
		clientToken := session.NewByJwt(networkN, userN, "ctfclientn", false, false).Client(devices[iceland], iceland).Testing_Sign()
		networkToken := session.NewByJwt(networkN, userN, "ctfclientn", false, false).Testing_Sign()
		otherToken := session.NewByJwt(networkM, userM, "ctfclientm", false, false).Testing_Sign()

		// a client token: its client, its two epochs and its two wins, the
		// pending root's not claimed and the finalized one's claimed
		response := ask(clientToken)
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "private, no-cache" || response.Header().Get("ETag") == "" {
			t.Fatalf("client token: %d %v %s", response.Code, response.Header(), response.Body.String())
		}
		result, wins := snCtfClientTestWins(t, response)
		// (the fixture records no census members, so its epochs say only which
		// epochs they are)
		if result.ClientId == nil || *result.ClientId != iceland.String() || len(result.Epochs) != 2 || result.Epochs[0].Epoch != 9 || result.Epochs[1].Epoch != 8 {
			t.Fatalf("client answer %s", response.Body.String())
		}
		requireSnCtfEqual(t, "client wins", wins, []string{
			fmt.Sprintf("9 provider is 1 %s false", iceland),
			fmt.Sprintf("8 provider is 2 %s true", iceland),
		})
		if again := ask(clientToken, "If-None-Match", response.Header().Get("ETag")); again.Code != http.StatusNotModified {
			t.Fatalf("revalidation %d", again.Code)
		}

		// the network's token and its API key: every client of the network,
		// the removed one included; never the other network's
		networkWins := []string{
			fmt.Sprintf("9 provider is 1 %s false", iceland),
			fmt.Sprintf("9 extender de 1 %s false", extender),
			fmt.Sprintf("9 extender de 2 %s false", removed),
			fmt.Sprintf("8 provider is 2 %s true", iceland),
		}
		for name, token := range map[string]string{"network token": networkToken, "API key": apiKey.ApiKey} {
			response := ask(token)
			if response.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", name, response.Code, response.Body.String())
			}
			result, wins := snCtfClientTestWins(t, response)
			if result.ClientId != nil || len(result.Epochs) != 0 {
				t.Fatalf("%s: client %v, %d epochs", name, result.ClientId, len(result.Epochs))
			}
			requireSnCtfEqual(t, name+" wins", wins, networkWins)
		}
		_, wins = snCtfClientTestWins(t, ask(otherToken))
		requireSnCtfEqual(t, "the other network's wins", wins, []string{fmt.Sprintf("9 provider is 2 %s false", other)})
		// the claim states asked for: only finalized paying roots, so only
		// the Iceland provider's coldkey at root 1 for N
		for _, needs := range source.needs {
			for _, need := range needs {
				if need.Coldkey != icelandColdkey || len(need.Roots) != 1 || need.Roots[0].Seq != 1 {
					t.Fatalf("need %+v", need)
				}
			}
		}

		// no token, or one that does not authenticate
		for _, token := range []string{"", "not-a-token", networkToken[:len(networkToken)-4] + "AAAA"} {
			response := ask(token)
			requireSnProblem(t, response, http.StatusUnauthorized, SnProblemUnauthorized)
			if response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("token %q: WWW-Authenticate %q", token, response.Header().Get("WWW-Authenticate"))
			}
		}
		// capture the flag off: unavailable, for a network's token too
		withSnCtfTestSource(t, nil)
		for _, token := range []string{clientToken, networkToken, apiKey.ApiKey} {
			requireSnProblem(t, ask(token), http.StatusServiceUnavailable, SnProblemUnavailable)
		}

		// On again, after the reaper has marked every idle client inactive and
		// hard-deleted the inactive ones, as it does 30 days after a
		// deactivation: the network's wins are still all of its clients'.
		withSnCtfTestSource(t, source)
		later := time.Now().Add(time.Hour)
		model.RemoveDisconnectedNetworkClients(ctx, later, later, later)
		var left int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client WHERE client_id = ANY($1::uuid[])`, []server.Id{iceland, extender, removed, other}).Scan(&left))
		})
		if left != 0 {
			t.Fatalf("%d of the clients' network_client rows left after the reaper", left)
		}
		for name, token := range map[string]string{"network token": networkToken, "API key": apiKey.ApiKey} {
			response := ask(token)
			if response.Code != http.StatusOK {
				t.Fatalf("%s after the reaper: %d %s", name, response.Code, response.Body.String())
			}
			_, wins := snCtfClientTestWins(t, response)
			requireSnCtfEqual(t, name+" wins after the reaper", wins, networkWins)
		}
		_, wins = snCtfClientTestWins(t, ask(otherToken))
		requireSnCtfEqual(t, "the other network's wins after the reaper", wins, []string{fmt.Sprintf("9 provider is 2 %s false", other)})
	})
}

// snCtfClaimsFixture is the CTF pool (2^32 + 1) of the ledger endpoint
// fixture: finalized roots 0 (epoch 8: A 600, B 400), 1 (epoch 9: A 1,600, B
// 400, C 1,000) and 2 (epoch 10: A 2,600, B 2,400, C 1,000), and pending root
// 3 (epoch 11: A 3,600), their objects published.
type snCtfClaimsFixture struct {
	*ledgerV2EndpointFixture
	pool  uint64
	roots []*ledger.Ledger
	// the view's roots, by root index; the pending one last
	viewRoots []*snCtfRoot
}

func newSnCtfClaimsFixture(t testing.TB) *snCtfClaimsFixture {
	f := &snCtfClaimsFixture{ledgerV2EndpointFixture: newLedgerV2EndpointFixture(t), pool: 1<<32 + 1}
	setCtfPoolTestSwitches(t, f.cfg, true, true)
	snCtfResetClaimedCache()
	t.Cleanup(snCtfResetClaimedCache)
	usage := func(rows ...any) []protocol.LedgerUsageRow {
		out := []protocol.LedgerUsageRow{}
		for i := 0; i < len(rows); i += 2 {
			out = append(out, protocol.LedgerUsageRow{ClientID: ledgerV2ClientId(byte(i + 1)), Coldkey: rows[i].([32]byte), UsageBytes: uint64(rows[i+1].(int))})
		}
		return out
	}
	var previous *ledger.Ledger
	for i, epoch := range []struct {
		epoch  uint64
		funded uint64
		rows   []protocol.LedgerUsageRow
	}{
		{8, 1_000, usage(ledgerV2A, 600_000, ledgerV2B, 400_000)},
		{9, 3_000, usage(ledgerV2A, 1, ledgerV2C, 1)},
		{10, 6_000, usage(ledgerV2A, 1, ledgerV2B, 2)},
		{11, 7_000, usage(ledgerV2A, 1)},
	} {
		built := f.build(f.pool, epoch.epoch, 8, previous, epoch.funded, epoch.rows)
		seq := uint64(i + 1)
		root := &snCtfRoot{Seq: seq, Epoch: epoch.epoch}
		if i < 3 {
			rootIndex := uint64(i)
			root.Finalized, root.RootIndex = true, &rootIndex
			f.mirror(f.pool, seq, built, &rootIndex)
		} else {
			f.mirror(f.pool, seq, built, nil)
		}
		f.roots = append(f.roots, built)
		f.viewRoots = append(f.viewRoots, root)
		previous = built
	}
	want := [][]merkleAccountWant{
		{{ledgerV2A, 600}, {ledgerV2B, 400}},
		{{ledgerV2A, 1_600}, {ledgerV2B, 400}, {ledgerV2C, 1_000}},
		{{ledgerV2A, 2_600}, {ledgerV2B, 2_400}, {ledgerV2C, 1_000}},
		{{ledgerV2A, 3_600}, {ledgerV2B, 2_400}, {ledgerV2C, 1_000}},
	}
	for i, built := range f.roots {
		accounts := []string{}
		for _, account := range built.Accounts {
			accounts = append(accounts, fmt.Sprintf("%x %s", account.Coldkey[:1], account.Cumulative.Dec()))
		}
		wantAccounts := []string{}
		for _, account := range want[i] {
			wantAccounts = append(wantAccounts, fmt.Sprintf("%x %d", account.coldkey[:1], account.cumulative))
		}
		requireSnCtfEqual(t, fmt.Sprintf("root %d's accounts", i), accounts, wantAccounts)
	}
	return f
}

type merkleAccountWant struct {
	coldkey    [32]byte
	cumulative uint64
}

// recordThrough records the finalized roots 0 … rootIndex as the driver's
// record step does: each root's new accounts and its coldkeys' deltas.
func (self *snCtfClaimsFixture) recordThrough(rootIndex int) {
	self.t.Helper()
	for i := 0; i <= rootIndex; i += 1 {
		if root := model.GetStLedgerV2Root(self.ctx, self.cfg.DeploymentKey(), self.pool, uint64(i+1)); root.RecordedTime != nil {
			continue
		}
		previous := 0
		if 0 < i {
			previous = len(self.roots[i-1].Accounts)
		}
		amounts := []*model.StLedgerV2Amount{}
		newColdkeys := [][32]byte{}
		for index, account := range self.roots[i].Accounts {
			delta := account.Cumulative
			if index < previous {
				delta.Sub(&account.Cumulative, &self.roots[i-1].Accounts[index].Cumulative)
			} else {
				newColdkeys = append(newColdkeys, account.Coldkey)
			}
			amounts = append(amounts, &model.StLedgerV2Amount{NoId: self.pool, Epoch: self.roots[i].Commit.Epoch, Coldkey: account.Coldkey, Amount: delta, RootIndex: uint64(i)})
		}
		if err := model.RecordStLedgerV2Root(self.ctx, self.cfg.DeploymentKey(), self.pool, uint64(i+1), uint64(previous), newColdkeys, amounts); err != nil {
			self.t.Fatal(err)
		}
	}
}

func (self *snCtfClaimsFixture) need(coldkey [32]byte, rootIndexes ...int) *snCtfClaimNeed {
	need := &snCtfClaimNeed{Coldkey: coldkey}
	for _, rootIndex := range rootIndexes {
		need.Roots = append(need.Roots, self.viewRoots[rootIndex])
	}
	return need
}

// The claim states as "claimed: seq=cumulative …", by coldkey's first byte;
// a coldkey left out reads "unread".
func snCtfClaimsTestStates(claims map[[32]byte]*snCtfAccountClaims, coldkeys ...[32]byte) map[string]string {
	states := map[string]string{}
	for _, coldkey := range coldkeys {
		claim, ok := claims[coldkey]
		if !ok {
			states[fmt.Sprintf("%x", coldkey[:1])] = "unread"
			continue
		}
		seqs := []uint64{}
		for seq := range claim.Cumulative {
			seqs = append(seqs, seq)
		}
		slices.Sort(seqs)
		state := claim.Claimed.Dec() + ":"
		for _, seq := range seqs {
			cumulative := claim.Cumulative[seq]
			state += fmt.Sprintf(" %d=%s", seq, cumulative.Dec())
		}
		states[fmt.Sprintf("%x", coldkey[:1])] = state
	}
	return states
}

// The claim state of each coldkey's CTF account: vault2's claimed at the
// finalized head, and its cumulative in each root it is asked for, from the
// record of each root's deltas below the first root not recorded yet, the
// latest finalized root's accounts, and a finalized root's own published
// accounts while the record has not reached it. A coldkey whose state
// disagrees with itself, or cannot be read, is left out; one whose account the
// records do not find has nothing paid, so its wins are null, never claimed;
// none is read while capture the flag is off.
func TestDbSnCtfAccountClaims(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnCtfClaimsFixture(t)
		source := &snCtfCoreSource{cfg: f.cfg}
		// D has no account in the pool
		ledgerV2D := ledgerV2Coldkey(0x0d)
		needs := []*snCtfClaimNeed{f.need(ledgerV2A, 2, 1, 0), f.need(ledgerV2B, 2, 0), f.need(ledgerV2C, 1), f.need(ledgerV2D, 0)}
		// A has claimed through root 1, B through root 0, C nothing
		f.chain.claimed[[2]uint64{f.pool, 0}] = *uint256.NewInt(1_600)
		f.chain.claimed[[2]uint64{f.pool, 1}] = *uint256.NewInt(400)
		// each read is the chain's now, not a value kept from a read before
		read := func() map[string]string {
			snCtfResetClaimedCache()
			return snCtfClaimsTestStates(source.AccountClaims(f.ctx, nil, needs), ledgerV2A, ledgerV2B, ledgerV2C, ledgerV2D)
		}
		want := map[string]string{
			"0a": "1600: 1=600 2=1600 3=2600",
			"0b": "400: 1=400 3=2400",
			"0c": "0: 2=1000",
			"0d": "0: 1=0",
		}
		// nothing recorded: root 2 by the ledger route's read, roots 0 and 1
		// by their published accounts
		requireSnCtfEqual(t, "nothing recorded", read(), want)
		// root 0 recorded
		f.recordThrough(0)
		requireSnCtfEqual(t, "root 0 recorded", read(), want)
		// roots 0 and 1 recorded, the latest finalized root not yet
		f.recordThrough(1)
		requireSnCtfEqual(t, "roots 0 and 1 recorded", read(), want)
		// every root recorded: the record and the latest root agree, and no
		// tree but the latest finalized root's is read
		f.recordThrough(2)
		resetLedgerV2Caches()
		f.builder.reads.Store(0)
		requireSnCtfEqual(t, "every root recorded", read(), want)
		if reads := f.builder.reads.Load(); reads != 1 {
			t.Fatalf("%d tree reads with every root recorded, want the latest finalized root's", reads)
		}
		// every account at one finalized block, read once, in one batch
		f.chain.fail["finalized"] = []error{nil, errors.New("a second finalized read")}
		requireSnCtfEqual(t, "one finalized read", read(), want)
		delete(f.chain.fail, "finalized")

		// vault2's claimed above the finalized cumulative: A is left out
		f.chain.claimed[[2]uint64{f.pool, 0}] = *uint256.NewInt(2_601)
		want["0a"] = "unread"
		requireSnCtfEqual(t, "claimed above the cumulative", read(), want)
		f.chain.claimed[[2]uint64{f.pool, 0}] = *uint256.NewInt(2_600)
		want["0a"] = "2600: 1=600 2=1600 3=2600"
		requireSnCtfEqual(t, "claimed through the latest root", read(), want)

		// the record does not add up to the latest finalized cumulative: B is
		// left out
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO st_ledger_v2_amount (deployment_key, no_id, epoch, coldkey, amount, root_index) VALUES ($1, $2, 7, $3, 1, 0)`,
				string(f.cfg.DeploymentKey()), int64(f.pool), ledgerV2B[:]))
		})
		want["0b"] = "unread"
		requireSnCtfEqual(t, "a record that does not add up", read(), want)

		// vault2 cannot be read once this instance has read it: past the time a
		// value is fresh, each account answers its last value read
		clock := snCtfClaimedNow
		t.Cleanup(func() { snCtfClaimedNow = clock })
		snCtfClaimedNow = func() time.Time { return time.Now().Add(2 * snCtfClaimedTtl) }
		f.chain.fail["claimed"] = []error{errors.New("rpc down"), errors.New("rpc down"), errors.New("rpc down"), errors.New("rpc down")}
		requireSnCtfEqual(t, "vault2 unreadable after a read", snCtfClaimsTestStates(source.AccountClaims(f.ctx, nil, needs), ledgerV2A, ledgerV2B, ledgerV2C, ledgerV2D), want)
		delete(f.chain.fail, "claimed")
		f.chain.fail["finalized"] = []error{errors.New("rpc down")}
		requireSnCtfEqual(t, "no finalized head after a read", snCtfClaimsTestStates(source.AccountClaims(f.ctx, nil, needs), ledgerV2A, ledgerV2B, ledgerV2C, ledgerV2D), want)
		delete(f.chain.fail, "finalized")
		snCtfClaimedNow = clock

		// vault2 cannot be read and this instance never read it: every account
		// with a finalized root is left out; D, which has none, needs no read
		f.chain.fail["claimed"] = []error{errors.New("rpc down"), errors.New("rpc down"), errors.New("rpc down"), errors.New("rpc down")}
		requireSnCtfEqual(t, "vault2 unreadable", read(), map[string]string{"0a": "unread", "0b": "unread", "0c": "unread", "0d": "0: 1=0"})
		delete(f.chain.fail, "claimed")
		// nor its finalized head
		f.chain.fail["finalized"] = []error{errors.New("rpc down")}
		requireSnCtfEqual(t, "no finalized head", read(), map[string]string{"0a": "unread", "0b": "unread", "0c": "unread", "0d": "0: 1=0"})
		delete(f.chain.fail, "finalized")

		// The state a root recorded between the account record's read and the
		// count of recorded accounts once left: the record without A while
		// the count covers A's index. The records find no account of A, so
		// nothing was paid to it, and every win of it is null, never claimed.
		server.Tx(f.ctx, func(tx server.PgTx) {
			for _, table := range []string{"st_ledger_v2_account", "st_ledger_v2_amount"} {
				server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM `+table+` WHERE deployment_key = $1 AND no_id = $2 AND coldkey = $3`,
					string(f.cfg.DeploymentKey()), int64(f.pool), ledgerV2A[:]))
			}
		})
		if count := model.GetStLedgerV2AccountCount(f.ctx, f.cfg.DeploymentKey(), f.pool); count != 3 {
			t.Fatalf("the count of recorded accounts is %d, want B's and C's to cover A's index", count)
		}
		claims := func() map[[32]byte]*snCtfAccountClaims {
			snCtfResetClaimedCache()
			return source.AccountClaims(f.ctx, nil, needs)
		}()
		requireSnCtfEqual(t, "an account the records do not find", snCtfClaimsTestStates(claims, ledgerV2A)["0a"], "0: 1=0 2=0 3=0")
		for i, root := range f.viewRoots[:3] {
			// A's win of the epoch of each finalized root, as much as A has in it
			cumulative := f.roots[i].Accounts[0].Cumulative
			win := &snCtfWin{Epoch: f.roots[i].Commit.Epoch, Award: &snCtfAward{Coldkey: ledgerV2A, Slot: cumulative}}
			if claimed := snCtfWinClaimed(win, root, claims[ledgerV2A]); claimed != nil {
				t.Fatalf("A's win paid by root %d reads claimed %t with its account not found, want null", root.Seq, *claimed)
			}
		}

		// capture the flag off: none is read
		setCtfPoolTestSwitches(t, f.cfg, true, false)
		if claims := source.AccountClaims(f.ctx, nil, needs); len(claims) != 0 {
			t.Fatalf("%d claim states read with capture the flag off", len(claims))
		}
	})
}
