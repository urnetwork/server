package handlers

// The SN25 validator data grant's refusals, renewal, concurrency and settings
// at the rate class mint, over the reserved fixture's real admission and the
// local database. The grant's clock is the mint's plus an offset the test
// sets, so no test waits for a grant to end.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The mint route over lane with the production data grant, run at the mint's
// time plus offset, sending each outcome to outcomes.
func newSnValidatorDataGrantEndpoint(tb testing.TB, lane SnReservedAttemptUploadLane, offset *atomic.Int64, outcomes chan<- string) string {
	tb.Helper()
	grant := func(ctx context.Context, networkId server.Id, hotkey [32]byte, now time.Time) string {
		outcome := controller.GrantSnValidatorData(ctx, networkId, hotkey, now.Add(time.Duration(offset.Load())))
		outcomes <- outcome
		return outcome
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveSnValidatorClass(w, r, lane, session.MintValidatorClass, grant, time.Now)
	}))
	tb.Cleanup(endpoint.Close)
	return endpoint.URL
}

// One admitted validator's mint, returning errors rather than failing the
// test, so it can run on any goroutine.
func snValidatorDataGrantMint(ctx context.Context, origin string, authority *snReservedAttemptAuthority, credential string) (int, error) {
	sessionHash, err := protocol.ValidatorAttemptUploadSessionHash(credential)
	if err != nil {
		return 0, err
	}
	intent := protocol.ValidatorClassIntent{ActivationHash: authority.digest, ReplicaNoID: 1, SessionHash: sessionHash, NotAfter: uint64(time.Now().Unix()) + 20}
	copy(intent.VPK[:], authority.key[ed25519.SeedSize:])
	header, err := intent.Sign(authority.key)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/sn/validator-class", nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set(protocol.ValidatorClassIntentHeader, header)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	return response.StatusCode, err
}

// Whether the hotkey holds a grant receipt.
func snValidatorDataGrantTestReceipt(tb testing.TB, hotkey [32]byte) bool {
	tb.Helper()
	found := false
	server.Db(tb.Context(), func(conn server.PgConn) {
		result, err := conn.Query(
			tb.Context(),
			`SELECT 1 FROM transfer_balance_grant_run WHERE run_id = $1 AND grant_kind = $2`,
			model.ValidatorDataGrantId(hotkey),
			model.GrantKindSn25Validator,
		)
		server.WithPgResult(result, err, func() {
			found = result.Next()
		})
	})
	return found
}

// The next outcome the endpoint reported.
func snValidatorDataGrantNextOutcome(tb testing.TB, outcomes <-chan string) string {
	tb.Helper()
	select {
	case outcome := <-outcomes:
		return outcome
	default:
		tb.Fatal("the mint asked for no data grant")
		return ""
	}
}

// A mint that is refused, with no class consent (400), with a network token
// (401), or with a consent the admission refuses (403), asks for no grant:
// the network has no balance and the hotkey no receipt.
func TestSnValidatorClassRefusedMintsGrantNoData(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		var offset atomic.Int64
		outcomes := make(chan string, 16)
		origin := newSnValidatorDataGrantEndpoint(tb, authority.start(tb, 1), &offset, outcomes)
		account, client := snAttemptUploadTestIdentity(tb)
		credential := client.Testing_Sign()
		networkCredential := account.Testing_Sign()

		noConsent, err := http.NewRequestWithContext(tb.Context(), http.MethodPost, origin+"/sn/validator-class", nil)
		if err != nil {
			tb.Fatal(err)
		}
		noConsent.Header.Set("Authorization", "Bearer "+credential)
		response, err := http.DefaultClient.Do(noConsent)
		if err != nil {
			tb.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			tb.Fatalf("a mint without a class consent = %d, want 400", response.StatusCode)
		}

		otherKey := ed25519.NewKeyFromSeed(append([]byte{0x42}, make([]byte, 31)...))
		for _, c := range []struct {
			name   string
			status int
			mint   func() (int, string)
		}{
			{name: "a network token", status: http.StatusUnauthorized, mint: func() (int, string) {
				return snValidatorClassTestRequest(tb, origin, authority, networkCredential, networkCredential, authority.key, 1)
			}},
			{name: "another session", status: http.StatusForbidden, mint: func() (int, string) {
				return snValidatorClassTestRequest(tb, origin, authority, credential, "another.session.credential", authority.key, 1)
			}},
			{name: "another key", status: http.StatusForbidden, mint: func() (int, string) {
				return snValidatorClassTestRequest(tb, origin, authority, credential, credential, otherKey, 1)
			}},
			{name: "another replica", status: http.StatusForbidden, mint: func() (int, string) {
				return snValidatorClassTestRequest(tb, origin, authority, credential, credential, authority.key, 2)
			}},
		} {
			if status, body := c.mint(); status != c.status {
				tb.Fatalf("%s: mint = %d %q, want %d", c.name, status, body, c.status)
			}
		}

		if len(outcomes) != 0 {
			tb.Fatalf("refused mints asked for %d data grants, want none", len(outcomes))
		}
		if balances := snValidatorDataGrantTestBalances(tb, client.NetworkId); len(balances) != 0 {
			tb.Fatalf("refused mints left %d balances on the network, want none", len(balances))
		}
		if snValidatorDataGrantTestReceipt(tb, authority.record.Hotkey) {
			tb.Fatal("refused mints claimed the hotkey's grant receipt")
		}
	})
}

// A mint after the validator's grant ended grants the next 30 days from that
// mint, and so does the first mint in a grant's last 2 days; the mints between
// grant nothing.
func TestSnValidatorClassMintAfterGrantEndsGrantsAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		var offset atomic.Int64
		outcomes := make(chan string, 16)
		origin := newSnValidatorDataGrantEndpoint(tb, authority.start(tb, 1), &offset, outcomes)
		_, client := snAttemptUploadTestIdentity(tb)
		credential := client.Testing_Sign()
		settings := model.DefaultValidatorDataGrantSettings()
		config := model.Pro()
		previous := config.ValidatorDataGrant
		config.ValidatorDataGrant = settings
		defer func() { config.ValidatorDataGrant = previous }()

		mint := func(at time.Duration, want string) {
			offset.Store(int64(at))
			if status, body := snValidatorClassTestRequest(tb, origin, authority, credential, credential, authority.key, 1); status != http.StatusOK {
				tb.Fatalf("an admitted mint at +%s = %d %q", at, status, body)
			}
			if outcome := snValidatorDataGrantNextOutcome(tb, outcomes); outcome != want {
				tb.Fatalf("the data grant of a mint at +%s is %s, want %s", at, outcome, want)
			}
		}
		day := 24 * time.Hour
		mint(0, controller.SnValidatorDataGrantCreated)
		mint(time.Hour, controller.SnValidatorDataGrantSkipped)
		mint(27*day, controller.SnValidatorDataGrantSkipped)
		// the first grant ended at +30 days
		mint(31*day, controller.SnValidatorDataGrantCreated)
		mint(31*day+time.Hour, controller.SnValidatorDataGrantSkipped)
		// the second grant ends at +61 days: 2 days after this mint
		mint(59*day, controller.SnValidatorDataGrantCreated)
		mint(59*day+time.Hour, controller.SnValidatorDataGrantSkipped)

		balances := snValidatorDataGrantTestBalances(tb, client.NetworkId)
		if len(balances) != 3 {
			tb.Fatalf("%d grants, want 3", len(balances))
		}
		for i, at := range []time.Duration{31 * day, 59 * day} {
			if gap := balances[i+1].startTime.Sub(balances[0].startTime); gap < at || at+time.Minute < gap {
				tb.Errorf("grant %d starts %s after the first, want the mint at +%s", i+1, gap, at)
			}
		}
		for _, balance := range balances {
			if balance.startBalanceByteCount != settings.BalanceByteCount || balance.endTime.Sub(balance.startTime) != settings.Duration {
				tb.Errorf("a grant is %d bytes for %s, want %d for %s", balance.startBalanceByteCount, balance.endTime.Sub(balance.startTime), settings.BalanceByteCount, settings.Duration)
			}
		}
	})
}

// Two admitted mints whose grants reach the claim together grant once. Both
// hold their network lock at the hook before either claims.
func TestSnValidatorClassConcurrentMintsGrantDataOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		var offset atomic.Int64
		outcomes := make(chan string, 16)
		origin := newSnValidatorDataGrantEndpoint(tb, authority.start(tb, 1), &offset, outcomes)
		_, client := snAttemptUploadTestIdentity(tb)
		credential := client.Testing_Sign()
		config := model.Pro()
		previous := config.ValidatorDataGrant
		config.ValidatorDataGrant = model.DefaultValidatorDataGrantSettings()
		defer func() { config.ValidatorDataGrant = previous }()

		const mintCount = 2
		var arrivals sync.WaitGroup
		arrivals.Add(mintCount)
		release := make(chan struct{})
		var arrivalCount atomic.Int32
		defer model.Testing_SetValidatorDataGrantBeforeClaim(func() {
			// a retried transaction passes the hook again, after the release
			if arrivalCount.Add(1) <= mintCount {
				arrivals.Done()
			}
			<-release
		})()

		statuses := make(chan int, mintCount)
		errs := make(chan error, mintCount)
		for i := 0; i < mintCount; i += 1 {
			go func() {
				status, err := snValidatorDataGrantMint(tb.Context(), origin, authority, credential)
				statuses <- status
				errs <- err
			}()
		}
		arrived := make(chan struct{})
		go func() {
			arrivals.Wait()
			close(arrived)
		}()
		select {
		case <-arrived:
		case <-time.After(time.Minute):
			close(release)
			tb.Fatal("the two mints' grants did not both reach the claim")
		}
		close(release)

		for i := 0; i < mintCount; i += 1 {
			if err := <-errs; err != nil {
				tb.Fatal(err)
			}
			if status := <-statuses; status != http.StatusOK {
				tb.Fatalf("a concurrent admitted mint = %d", status)
			}
		}
		counts := map[string]int{}
		for i := 0; i < mintCount; i += 1 {
			counts[snValidatorDataGrantNextOutcome(tb, outcomes)] += 1
		}
		if counts[controller.SnValidatorDataGrantCreated] != 1 || counts[controller.SnValidatorDataGrantSkipped] != 1 {
			tb.Fatalf("concurrent mints' grant outcomes %v, want one created and one skipped", counts)
		}
		if balances := snValidatorDataGrantTestBalances(tb, client.NetworkId); len(balances) != 1 {
			tb.Fatalf("concurrent mints left %d grants, want 1", len(balances))
		}
	})
}

// The production route reads the grant's size and duration from pro.yml
// validator_grant: an override changes the grant, and settings that grant
// nothing leave the mint answered with no grant.
func TestSnValidatorClassDataGrantFollowsProConfig(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		endpoint := httptest.NewServer(SnValidatorClassWithReserved(authority.start(tb, 1)))
		tb.Cleanup(endpoint.Close)
		config := model.Pro()
		previous := config.ValidatorDataGrant
		defer func() { config.ValidatorDataGrant = previous }()

		config.ValidatorDataGrant = model.ValidatorDataGrantSettings{
			BalanceByteCount: 7 * model.Gib,
			Duration:         7 * 24 * time.Hour,
			RenewBefore:      24 * time.Hour,
			Timeout:          5 * time.Second,
		}
		_, client := snAttemptUploadTestIdentity(tb)
		credential := client.Testing_Sign()
		if status, body := snValidatorClassTestRequest(tb, endpoint.URL, authority, credential, credential, authority.key, 1); status != http.StatusOK {
			tb.Fatalf("an admitted mint = %d %q", status, body)
		}
		balances := snValidatorDataGrantTestBalances(tb, client.NetworkId)
		if len(balances) != 1 || balances[0].startBalanceByteCount != 7*model.Gib || balances[0].endTime.Sub(balances[0].startTime) != 7*24*time.Hour {
			tb.Fatalf("the grant under the override is %+v, want one of 7 GiB for 7 days", balances)
		}

		// another validator network, under settings that grant nothing
		config.ValidatorDataGrant.BalanceByteCount = 0
		_, otherClient := snAttemptUploadTestIdentity(tb)
		otherCredential := otherClient.Testing_Sign()
		if status, body := snValidatorClassTestRequest(tb, endpoint.URL, authority, otherCredential, otherCredential, authority.key, 1); status != http.StatusOK {
			tb.Fatalf("an admitted mint with the grant disabled = %d %q", status, body)
		}
		if balances := snValidatorDataGrantTestBalances(tb, otherClient.NetworkId); len(balances) != 0 {
			tb.Fatalf("a disabled grant left %d balances", len(balances))
		}
	})
}

// A grant that cannot be written leaves the mint answered: the token is the
// class, and the next mint asks again.
func TestSnValidatorClassMintAnswersWhenTheGrantFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		lane := authority.start(tb, 1)
		var failing atomic.Bool
		failing.Store(true)
		outcomes := make(chan string, 16)
		grant := func(ctx context.Context, networkId server.Id, hotkey [32]byte, now time.Time) string {
			if failing.Load() {
				// a grant whose database work cannot run
				canceled, cancel := context.WithCancelCause(ctx)
				cancel(errors.New("synthetic database outage"))
				ctx = canceled
			}
			outcome := controller.GrantSnValidatorData(ctx, networkId, hotkey, now)
			outcomes <- outcome
			return outcome
		}
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveSnValidatorClass(w, r, lane, session.MintValidatorClass, grant, time.Now)
		}))
		tb.Cleanup(endpoint.Close)
		_, client := snAttemptUploadTestIdentity(tb)
		credential := client.Testing_Sign()
		config := model.Pro()
		previous := config.ValidatorDataGrant
		config.ValidatorDataGrant = model.DefaultValidatorDataGrantSettings()
		defer func() { config.ValidatorDataGrant = previous }()

		if status, body := snValidatorClassTestRequest(tb, endpoint.URL, authority, credential, credential, authority.key, 1); status != http.StatusOK {
			tb.Fatalf("a mint whose grant fails = %d %q, want the token", status, body)
		}
		if outcome := snValidatorDataGrantNextOutcome(tb, outcomes); outcome != controller.SnValidatorDataGrantFailed {
			tb.Fatalf("the failing grant's outcome is %s", outcome)
		}
		if balances := snValidatorDataGrantTestBalances(tb, client.NetworkId); len(balances) != 0 || snValidatorDataGrantTestReceipt(tb, authority.record.Hotkey) {
			tb.Fatalf("a failed grant left %d balances or its receipt", len(balances))
		}

		failing.Store(false)
		if status, body := snValidatorClassTestRequest(tb, endpoint.URL, authority, credential, credential, authority.key, 1); status != http.StatusOK {
			tb.Fatalf("the next mint = %d %q", status, body)
		}
		if outcome := snValidatorDataGrantNextOutcome(tb, outcomes); outcome != controller.SnValidatorDataGrantCreated {
			tb.Fatalf("the next mint's grant is %s, want created", outcome)
		}
	})
}
