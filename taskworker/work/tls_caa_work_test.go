// certs' CAA checks chain (TLS-M6-04): scheduled once and only in a binary
// that links custom hosts, idle with its cursor kept while nothing is
// installed, a pass's verdicts counted and its cursor carried through the
// task's args, and the install refusing to run with no operator zone. The
// checks run on scripted parts here; certsservice's tests hold the walk and
// the probe, and model's the store.
package work

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const testTlsCaaPin = `0 issue "ca.example; accounturi=https://ca.example/acme/acct/1"`

// A resolver whose every name sits in customer.example on one server, the pin
// at the domain, and whose authoritative answers hold nothing.
type testTlsCaaResolver struct{}

func (testTlsCaaResolver) LookupCaa(ctx context.Context, name string) (*sidecar.TlsCaaAnswer, error) {
	answer := &sidecar.TlsCaaAnswer{
		Hops: []*sidecar.TlsCaaHop{{Name: name, Zone: "customer.example", Nameservers: []string{"ns1.provider.example"}}},
	}
	if name == "customer.example" {
		answer.Records = []string{testTlsCaaPin}
	}
	return answer, nil
}

func (testTlsCaaResolver) AuthoritativeCaa(ctx context.Context, name string, nameservers []string) (map[string][]string, error) {
	answers := map[string][]string{}
	for _, nameserver := range nameservers {
		answers[nameserver] = nil
	}
	return answers, nil
}

// The canary route, always ready.
type testTlsCaaDns struct {
	stateLock sync.Mutex
	serial    uint64
}

func (self *testTlsCaaDns) PublishCaaCanary(ctx context.Context, owner string, caa []string, replaces string) (uint64, string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.serial += 1
	return self.serial, "00112233445566778899aabbccddeeff", nil
}

func (self *testTlsCaaDns) DeleteCaaCanary(ctx context.Context, owner string) (uint64, error) {
	return 0, nil
}

func (self *testTlsCaaDns) Ready(ctx context.Context, serial uint64) (certsservice.CertsDnsReadiness, error) {
	return certsservice.CertsDnsReady, nil
}

// One pending host and one active host due now.
type testTlsCaaStore struct {
	stateLock sync.Mutex
	verdicts  map[string]bool
}

func (self *testTlsCaaStore) ListCaaPending(ctx context.Context, afterHost string, count int) ([]*certsservice.CertsCaaHost, error) {
	if afterHost != "" {
		return nil, nil
	}
	return []*certsservice.CertsCaaHost{{Host: "app.customer.example", Owner: "web.net1.root.example", Pending: true}}, nil
}

func (self *testTlsCaaStore) ListCaaDue(ctx context.Context, lifetimeFraction float64, since time.Time, until time.Time, afterHost string, count int) ([]*certsservice.CertsCaaHost, error) {
	if !since.IsZero() {
		return nil, nil
	}
	return []*certsservice.CertsCaaHost{{Host: "www.customer.example", Owner: "www.net1.root.example"}}, nil
}

func (self *testTlsCaaStore) GetCaaHost(ctx context.Context, host string) (*certsservice.CertsCaaHost, error) {
	return nil, nil
}

func (self *testTlsCaaStore) ApplyCaaVerdict(ctx context.Context, host string, pass bool) (certsservice.CertsCaaOutcome, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.verdicts[host] = pass
	if host == "app.customer.example" {
		return certsservice.CertsCaaOutcomeActivated, nil
	}
	return certsservice.CertsCaaOutcomeChecked, nil
}

// Runs function, moving clock to each timer it sets, until it returns.
func testTlsCaaDrive[T any](clock *sidecartest.FakeClock, function func() T) T {
	done := make(chan T, 1)
	go func() {
		done <- function()
	}()
	for {
		select {
		case result := <-done:
			return result
		default:
		}
		pending, added := clock.WatchTimers()
		if 0 < len(pending) {
			clock.AdvanceTo(pending[0])
			continue
		}
		select {
		case result := <-done:
			return result
		case <-added:
		}
	}
}

func TestTlsCaaChecksScheduledOnceAndOnlyWhenLinked(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		for range 2 {
			server.Tx(ctx, func(tx server.PgTx) {
				ScheduleTlsCaaChecks(clientSession, tx)
			})
		}
		want := 0
		if tlsCaaChecksLinked() {
			want = 1
		}
		var count int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("tls_caa_checks").String(),
			).Scan(&count))
		})
		if count != want {
			t.Fatalf("%d runs queued, want %d", count, want)
		}
	})
}

func TestTlsCaaChecksIdleKeepTheirCursor(t *testing.T) {
	SetTlsCaaChecks(nil)
	ctx := context.Background()
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	cursor := &certsservice.CertsCaaCursor{AfterPendingHost: "kept.customer.example"}
	result, err := TlsCaaChecks(&TlsCaaChecksArgs{Cursor: cursor}, clientSession)
	if err != nil || !result.Idle || result.Next != cursor || result.Failed {
		t.Fatalf("%+v, %v", result, err)
	}
	// a part missing installs nothing
	if installed := setTlsCaaChecks(&TlsCaaCheckDeps{Caa: &certsservice.CertsCaaDeps{}}); installed != nil || tlsCaaCheckDeps.Load() != nil {
		t.Fatalf("installed %+v", installed)
	}
}

func TestTlsCaaChecksRunAPassAndCarryItsCursor(t *testing.T) {
	if !tlsCaaChecksLinked() {
		return
	}
	ctx := context.Background()
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	settings := certsservice.DefaultCertsSettings()
	settings.RootDomain = "root.example"
	service, err := certsservice.NewCertsService(ctx, host.ServiceHost(), sidecartest.NewFakeCertsStore(nil), nil, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	store := &testTlsCaaStore{verdicts: map[string]bool{}}
	install := func(operatorZones []string) func() {
		return installTlsCaaChecks(&tlsCaaChecksBuild{
			service:       service,
			resolver:      testTlsCaaResolver{},
			store:         store,
			operatorZones: operatorZones,
		})
	}
	// no operator zone: nothing to refuse, so nothing is installed
	install(nil)
	if tlsCaaCheckDeps.Load() != nil {
		t.Fatal("installed with no operator zone")
	}
	uninstall := install([]string{"root.example"})
	defer uninstall()
	deps := tlsCaaCheckDeps.Load()
	if deps == nil {
		t.Fatal("not installed")
	}
	// the install's canary client is certs' DNS client, which this test stands in for
	deps.Caa.Dns = &testTlsCaaDns{}

	result := testTlsCaaDrive(clock, func() *TlsCaaChecksResult {
		result, err := TlsCaaChecks(&TlsCaaChecksArgs{}, clientSession)
		if err != nil {
			t.Error(err)
		}
		return result
	})
	if result.Idle || result.Failed || result.ActivatedCount != 1 || result.CheckedCount != 1 || result.InconclusiveCount != 0 {
		t.Fatalf("%+v", result)
	}
	if !store.verdicts["app.customer.example"] || !store.verdicts["www.customer.example"] {
		t.Errorf("verdicts %v", store.verdicts)
	}
	// the cursor goes through the task's args as JSON, the due window closed behind it
	argsJson, err := json.Marshal(&TlsCaaChecksArgs{Cursor: result.Next})
	if err != nil {
		t.Fatal(err)
	}
	args := &TlsCaaChecksArgs{}
	if err := json.Unmarshal(argsJson, args); err != nil {
		t.Fatal(err)
	}
	if args.Cursor == nil || args.Cursor.DueSince.IsZero() || !args.Cursor.DueUntil.IsZero() || args.Cursor.AfterPendingHost != "" {
		t.Fatalf("cursor %s", argsJson)
	}
	// the next pass judges the due host no more
	next := testTlsCaaDrive(clock, func() *TlsCaaChecksResult {
		result, _ := TlsCaaChecks(args, clientSession)
		return result
	})
	if next.CheckedCount != 0 || next.ActivatedCount != 1 {
		t.Fatalf("next pass %+v", next)
	}
	uninstall()
	if tlsCaaCheckDeps.Load() != nil {
		t.Fatal("the uninstall left the checks installed")
	}
}
