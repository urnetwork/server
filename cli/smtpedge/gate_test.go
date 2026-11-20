// T2 for server/cli/smtpedge (SMTP-M9-20): the block scrubs its logs first and reads its urls from its environment;
// its gate loads a network once on a miss and answers from memory, its unwired row answering not loaded and §21.5's
// breaker following its source within a refresh (ABUSE-M9-02); and the block is not ready while a seam is missing, a
// sending address with no FCrDNS included. The X-UR-Ref key file's reader is model's (smtp_x_ur_ref_key_file.go),
// tested there. Time is a fake clock and nothing waits on a sleep.

package main

import (
	"context"
	"errors"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

func TestMainScrubsLogsFirst(t *testing.T) {
	if err := sidecarhost.CheckMainScrubsLogsFirst("main.go"); err != nil {
		t.Fatal(err)
	}
}

// A deployed block is given api's base URL in its environment, $UR_API_URL, from smtpedge.yml's env_vars, which
// -api_url reads as its default (sidecarhost.ServiceUrlFlag).
func TestApiUrlFlagDefaultsToTheBlocksEnvironment(t *testing.T) {
	if err := sidecarhost.CheckServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sidecarhost.ApiUrlEnvName, "https://api.example.com")
	if apiUrl.Url() != "https://api.example.com" {
		t.Fatalf("-api_url reads %q", apiUrl.Url())
	}
}

// A deployed block is given the sockets service's base URL in its environment, $UR_SOCKETS_URL, from smtpedge.yml's
// env_vars, which -sockets_url reads as its default (sidecarhost.ServiceUrlFlag).
func TestSocketsUrlFlagDefaultsToTheBlocksEnvironment(t *testing.T) {
	if err := sidecarhost.CheckServiceUrlFlag(flag.CommandLine, "sockets_url", sidecarhost.SocketsUrlEnvName); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sidecarhost.SocketsUrlEnvName, "https://sockets.example.com")
	if socketsUrl.Url() != "https://sockets.example.com" {
		t.Fatalf("-sockets_url reads %q", socketsUrl.Url())
	}
}

// A gate source counting its loads.
type testGateSource struct {
	stateLock sync.Mutex
	loads     map[sidecar.Id]int
	facts     *smtpedgeservice.SmtpGateFacts
	// §21.5's state as the source reads it; nil is a read that failed
	breaker *smtpedgeservice.SmtpBreakerState
	// signalled, when set, each time a refresh has read the breaker and gone on to Community's limits
	refreshed chan struct{}
}

func (self *testGateSource) Breaker(ctx context.Context) (*smtpedgeservice.SmtpBreakerState, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.breaker == nil {
		return nil, errors.New("not there")
	}
	return &smtpedgeservice.SmtpBreakerState{Level: self.breaker.Level, ListedAddresses: slices.Clone(self.breaker.ListedAddresses)}, nil
}

func (self *testGateSource) setBreaker(breaker *smtpedgeservice.SmtpBreakerState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.breaker = breaker
}

func (self *testGateSource) Facts(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpGateFacts, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.loads[networkId] += 1
	if self.facts == nil {
		return nil, errors.New("not there")
	}
	kept := *self.facts
	return &kept, nil
}

func (self *testGateSource) CommunityLimits(ctx context.Context) (*sidecar.QuotaLimits, error) {
	select {
	case self.refreshed <- struct{}{}:
	default:
	}
	return &sidecar.QuotaLimits{Tier: "free", Limits: map[string]int64{"smtp_connections_per_day": 5}, Enforced: true}, nil
}

func (self *testGateSource) loadCount(networkId sidecar.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.loads[networkId]
}

func TestHostGateLoadsOnAMissAndAnswersFromMemory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	source := &testGateSource{loads: map[sidecar.Id]int{}, facts: &smtpedgeservice.SmtpGateFacts{Label: "n1", MailEnableTime: clock.Now().Add(-48 * time.Hour)}}
	gate := newHostGate(ctx, source, clock, defaultHostGateSettings())
	networkId := sidecar.NewId()
	// a miss answers not loaded and asks for one load, however many preambles miss before it lands
	for range 3 {
		if facts, ok := gate.Facts(networkId); ok || facts != nil {
			t.Fatal("a cold gate answered")
		}
	}
	cancel()
	gate.wait()
	if count := source.loadCount(networkId); count != 1 {
		t.Fatalf("%d loads for one network", count)
	}
	facts, ok := gate.Facts(networkId)
	if !ok || facts.Label != "n1" {
		t.Fatalf("facts %+v, %v", facts, ok)
	}
	// a copy, so a caller's change never reaches the memory
	facts.Label = "changed"
	if again, _ := gate.Facts(networkId); again.Label != "n1" {
		t.Fatal("the gate handed out its own facts")
	}
	if limits, ok := gate.CommunityLimits(); !ok || limits.Tier != "free" || !limits.Enforced {
		t.Fatalf("community %+v, %v", limits, ok)
	}
}

// Community's limits carry enforce_sidecar_limits's state as NetworkQuotaLimits sets it, beside the plan, which
// they report whatever it says (owner decision 520).
func TestModelGateSourceCommunityLimitsCarryTheSwitch(t *testing.T) {
	source := &modelGateSource{}
	for _, enforced := range []bool{false, true} {
		restore := model.Testing_SetEnforceSidecarLimits(enforced)
		limits, err := source.CommunityLimits(context.Background())
		restore()
		if err != nil || limits.Tier != string(model.NetworkTierFree) || limits.Enforced != enforced {
			t.Errorf("switch %t: %+v, %v", enforced, limits, err)
		}
	}
}

// §21.5's breaker state reaches the gate within one refresh, off every preamble's path: a level and a listed
// sending address the source reads are the gate's after the next FactsRefreshInterval, the listed address is out of
// the sending set's view and nowhere else in the gate, and a read that fails leaves the breaker not loaded, never
// the state a refresh ago.
func TestHostGateLoadsTheBreakerOnItsRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	listed := netip.MustParseAddr("192.0.2.10")
	source := &testGateSource{
		loads:     map[sidecar.Id]int{},
		breaker:   &smtpedgeservice.SmtpBreakerState{Level: smtpedgeservice.SmtpBreakerLevelRestrict},
		refreshed: make(chan struct{}, 16),
	}
	settings := defaultHostGateSettings()
	gate := newHostGate(ctx, source, clock, settings)
	defer gate.wait()
	defer cancel()
	// one refresh has read the breaker once it goes on to Community's limits, then waits for the next
	awaitRefresh := func() {
		t.Helper()
		<-source.refreshed
		if err := clock.AwaitTimers(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	awaitRefresh()
	state, ok := gate.Breaker()
	if !ok || state.Level != smtpedgeservice.SmtpBreakerLevelRestrict || len(state.ListedAddresses) != 0 {
		t.Fatalf("after the first refresh: %+v, %t", state, ok)
	}

	// the breaker trips to level 2 with a sending address listed: the gate has it one refresh on, and not before
	source.setBreaker(&smtpedgeservice.SmtpBreakerState{Level: smtpedgeservice.SmtpBreakerLevelCommunity, ListedAddresses: []netip.Addr{listed}})
	if state, _ := gate.Breaker(); state.Level != smtpedgeservice.SmtpBreakerLevelRestrict {
		t.Fatalf("before the refresh the gate read the source: %+v", state)
	}
	clock.Advance(settings.FactsRefreshInterval)
	awaitRefresh()
	state, ok = gate.Breaker()
	if !ok || state.Level != smtpedgeservice.SmtpBreakerLevelCommunity || !state.Listed(listed) || state.Listed(netip.MustParseAddr("192.0.2.11")) {
		t.Fatalf("one refresh on: %+v, %t", state, ok)
	}
	// a copy, so a caller's change never reaches the memory
	state.Level = smtpedgeservice.SmtpBreakerLevelNone
	state.ListedAddresses[0] = netip.MustParseAddr("192.0.2.99")
	if again, _ := gate.Breaker(); again.Level != smtpedgeservice.SmtpBreakerLevelCommunity || !again.Listed(listed) {
		t.Fatal("the gate handed out its own breaker state")
	}
	// the listing is the sending set's alone: no network's facts and no limits carry the address
	if limits, ok := gate.CommunityLimits(); !ok || len(limits.Limits) != 1 {
		t.Fatalf("community %+v, %t", limits, ok)
	}

	// a read that fails leaves nothing loaded: TRY_LATER, not the level of a refresh ago
	source.setBreaker(nil)
	clock.Advance(settings.FactsRefreshInterval)
	awaitRefresh()
	if state, ok := gate.Breaker(); ok || state != nil {
		t.Fatalf("after a failed read the gate kept %+v", state)
	}
	// and it loads again once the source reads
	source.setBreaker(&smtpedgeservice.SmtpBreakerState{Level: smtpedgeservice.SmtpBreakerLevelAll})
	clock.Advance(settings.FactsRefreshInterval)
	awaitRefresh()
	if state, ok := gate.Breaker(); !ok || state.Level != smtpedgeservice.SmtpBreakerLevelAll {
		t.Fatalf("after the source read again: %+v, %t", state, ok)
	}
}

// mail_send has no source in this block yet, and a breaker whose source cannot read is not loaded: TRY_LATER.
func TestHostGateUnwiredRowsAnswerNotLoaded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := newHostGate(ctx, &testGateSource{loads: map[sidecar.Id]int{}}, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), defaultHostGateSettings())
	defer gate.wait()
	defer cancel()
	if decision := gate.MaySendMail(sidecar.NewId(), sidecar.NewId(), time.Now()); decision.Allowed || decision.Reason != sidecar.AclReasonNoState {
		t.Fatalf("mail_send %+v", decision)
	}
	if state, ok := gate.Breaker(); ok || state != nil {
		t.Fatal("a breaker whose source cannot read answered")
	}
}

// The block is not ready while a seam is missing, and says which; a sending address with no FCrDNS leaves the
// engine not ready either.
func TestBlockNotReadyWithoutItsSeams(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(`
root: root.example
report_host: rpt.report.example
helo_domain: out.report.example
sending_addresses:
  - {address: 192.0.2.10, helo: o1.out.report.example}
`), 0)
	if err != nil {
		t.Fatal(err)
	}
	log := &testLog{}
	block, err := newSmtpEdgeHost(ctx, defaultHostSettings(config), &hostDeps{
		Quota: host.Quota,
		Redis: host.Redis,
		Gates: host.Gates,
		Clock: clock,
		Log:   log,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer block.Close()
	if block.Ready() {
		t.Fatal("a block with no store, gate, resolver, verifier or client is ready")
	}
	missing := block.Missing()
	for _, want := range []string{"the store", "the send gate", "MX resolver", "PTR resolver", "the proof verifier", "the platform client"} {
		if !slices.ContainsFunc(missing, func(text string) bool { return strings.Contains(text, want) }) {
			t.Fatalf("missing %q does not name %q", missing, want)
		}
	}
	// the dialing sidecar is each admitted ticket's dialer_sidecar_id, no seam of the block's (QD-989)
	if slices.ContainsFunc(missing, func(text string) bool { return strings.Contains(text, "sidecar") }) {
		t.Fatalf("missing %q names a dialer-to-sidecar seam", missing)
	}
	if lines := log.Lines(); len(lines) != 1 || !strings.Contains(lines[0], "not ready without") {
		t.Fatalf("the block logged %q", lines)
	}
}

// Every line logged.
type testLog struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testLog) add(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testLog) Lines() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

func (self *testLog) Info(args ...any)                    { self.add("info") }
func (self *testLog) Infof(format string, args ...any)    { self.add(format) }
func (self *testLog) Warningf(format string, args ...any) { self.add(format) }
func (self *testLog) Errorf(format string, args ...any)   { self.add(format) }
func (self *testLog) V(level int32) sidecar.Verbose       { return testVerbose{} }

type testVerbose struct{}

func (testVerbose) Enabled() bool                    { return false }
func (testVerbose) Info(args ...any)                 {}
func (testVerbose) Infof(format string, args ...any) {}

// config/local's smtpedge.yml loads as the block reads it, its names documentation names and its addresses
// RFC 5737's, and its root and report host are mxedge.yml's platform ones, which the platform block's intake
// requires of the same file.
func TestLocalSmtpEdgeConfigLoads(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "local", "smtpedge.yml"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := smtpedgeservice.LoadSmtpEdgeConfig(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	documentation := netip.MustParsePrefix("192.0.2.0/24")
	for _, address := range config.Settings.SendingAddresses {
		if !documentation.Contains(address.Address) || !strings.HasSuffix(address.HeloName, ".example") {
			t.Fatalf("a sending address that is not documentation's: %+v", address)
		}
	}
	mxedge, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "local", "mxedge.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"platform_root: " + config.Settings.Root, "platform_report_host: " + config.Settings.ReportHostName} {
		if !strings.Contains(string(mxedge), line) {
			t.Fatalf("mxedge.yml has no %q", line)
		}
	}
}
