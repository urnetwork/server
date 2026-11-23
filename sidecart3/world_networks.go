package sidecart3

// A T3 world of networks (the end-to-end tests' clients; §22.4's T3, §3.7's F1): many full sidecars in each of several
// networks, each a T3Sidecar enrolled through T3Api from an auth key its network's owner made, Online on server's
// exchange, and the services a test serves reached at their origins (T3Api.ServeService), so a test drives what
// production gives a device end to end. A T3 Net's AddSidecar still refuses: its helpers stand on T2's fake api.
//
//	stores    one T3 Net (NewNet): its buckets on the local stack's MinIO, server's Redis, its capture and its fakes;
//	          the world's hosts run on a copy of its ServiceHost on real time (Host), since server's api, exchange and
//	          oauth keep real time and judge every token and proof by it
//	api       T3Api over server's routes, with what api's process installs over its gate cache
//	          (controller.installApiAuthorization and its projection part): server's sequencer over the Net's first
//	          metadata cluster (controller.NewHostedConfigLogSequencer), a gate cache over it, the exchange's scopes
//	          from that cache (oauth.NewProjectionExchangeScopeSource), so a service token carries exactly what §8's
//	          table grants at the log's newest state, the approval gate's states, the dns scopes' labels, the smtp
//	          routes' §8 checks, the edge logins' source, rev/'s store on the first cluster and the issuer's worker that
//	          claims its tasks, and the contract gate when sidecar.yml's acl_contract_gate is on as the world starts; the
//	          sequencer and rev/'s store spend on the Net's fake write budget, or with ApiWriteBudget on api's own over
//	          the world's Redis, which reports its spend beside the blocks' as api's process does
//	limits    §7.9's append limits and §20.4's caps run, on the numbers SidecarLimits gives pro.yml's sidecar keys for
//	          the world's life (§20.4's Community column by default), since the test stack's pro.yml carries none;
//	          AdjustConfigLog may still take them off for a test that needs it
//	oauth     the issuer is the sidecars' api origin and WARP_DOMAIN the world's domain while it runs, so a DPoP proof's
//	          htu and a service's origin are what server checks them against; the svc- key and the access tokens' key
//	          are throwaways the world makes, never vault's, and every host the world builds verifies tokens and rev/
//	          under its own verifier of the svc- key, as a block given HostDeps.ServiceTokens does, never the process's
//	networks  each a personal network in preview_networks, funded in the Net's quota and in a transfer balance of
//	          server's, and in every gated service's gate
//	          (AddNetwork): its first sidecar bootstraps it, signing the Genesis and base 1 through api (Bootstrap),
//	          and admits the others (Admit), each then following the log through its own token with its own key
//	          client; statements a device files go through POST /network/config/delta with its token (FileStatement)
//	keys      sidecartest's FakeGkm on the Net's real stores, writing each generation's GenRotate where evs's gate reads
//	          it, one client a device, handed each state its follower verifies; at KF1 the world's client also stores
//	          the repo's GroupForm, signed by the device's identity key, where gkm stores it and evs's activate route
//	          reads it (t3WorldGkmClient): gkm's own host and the MLS key client are not this world's (the file's gap
//	          list)
//	config    the config repo's heads are sidecartest's FakeConfigHeads in memory, one per network, as S1's genesis
//	          clause keeps them: the config group's KF1 on gkm is not this world's either
//
// Every wait is on a monitor, a follow or a gate's load, bounded by SyncTimeout in real time only to fail a broken run.
// What the world installs process-wide (the sequencer, the exchange's source and api's other installs, oauth's issuer
// and keys, WARP_DOMAIN, sidecar.yml, pro.yml's sidecar limits) is restored by Close, so worlds run one at a time, as
// every T3 test does. Its sidecars run until it closes, whatever context a call that added them carried. Safe for
// concurrent use once made.
//
// keys with RealKeys: each device's real key client at the gkm origin and its EvsClient over it, the config repo's
// heads on evs, and each network bootstrapped by its first owner's follower, as ur daemon runs them (world_keys.go).
// A joining device takes the config repo's key only by a late delivery that gkm's hint by sidecar id asks for, which
// gkm's own hint writer cannot address, and a repo's creation is refused by gkm's cached 404 of its placement
// (sidecare2e's world_keys_test.go has the evidence), so RealKeys is off by default, and a key reaches a device through
// FakeGkm's deliveries.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/task"
)

// What a T3 world runs with.
type T3WorldSettings struct {
	// The T3 Net the world's stores, capture and fakes are. nil: DefaultSettings.
	Net *Settings
	// T3Api's and the sidecars'. Their origins must sit under Domain. nil: DefaultT3SidecarSettings.
	Sidecar *T3SidecarSettings
	// Server's exchange the sidecars connect on. nil: DefaultExchangeSettings.
	Exchange *ExchangeSettings
	// The domain the origins sit under, WARP_DOMAIN while the world runs: sidecart3.example, a reserved name nothing
	// resolves. The token exchange issues for https://<service>.<domain> alone, and DPoP's api origin is
	// https://api.<domain>.
	Domain string
	// Each gate cache's: api's and each service's the world serves. nil: sidecar.DefaultAclGateSettings with its poll
	// off and no wait after a failed load, so a load follows a statement at once and the world's syncs, not a timer,
	// move each cache.
	Gate *sidecar.AclGateSettings
	// The services whose gates the world opens to every network of its, on the Net's sidecar.yml (sidecartest's
	// FakeServiceGates): evs and gkm.
	GatedServices []string
	// sidecar.yml as the world puts it for the process while it runs, given each network's id as preview_networks
	// lists it, quoted: T3WorldPreviewSidecarYaml, preview_networks alone. A world of hosts adds each service's gate.
	SidecarYaml func(previewNetworkIds []string) string
	// pro.yml's sidecar limits the world's api and hosts run under for its life (§20.4), which the test stack's
	// pro.yml does not carry. nil leaves the process's pro.yml as it is, where every sidecar limit reads zero.
	SidecarLimits *T3WorldSidecarLimits
	// What the world's sequencer runs with past api's own (model.NewApiConfigLogSettings), nil for nothing: §7.9's
	// append limits and §20.4's admission caps run on SidecarLimits' numbers. A test that needs them off clears the
	// limiter and both caps here.
	AdjustConfigLog func(settings *model.ConfigLogSettings)
	// What each network holds in the Net's quota, and in a transfer balance of server's that api's quota surface reads:
	// 1 TiB, past what any test writes.
	NetworkBalanceByteCount int64
	// How long that transfer balance is in window from the network's creation: 24 h, past any test.
	NetworkBalanceDuration time.Duration
	// How long an auth key an owner makes for one enrollment lives: 10 min.
	AuthKeyDuration time.Duration
	// How many sidecars enroll at once: 4.
	EnrollConcurrency int
	// How long a statement a device files lives past now, its header's not_after: 1 h, inside §7.9's 7 days.
	StatementLife time.Duration
	// How long, in real time, a wait for a gate's load, a follower's catch-up or a sidecar's standing may take: 60 s,
	// only a broken run's bound.
	SyncTimeout time.Duration
	// Whether each device's keys go through gkm's host with its own real key client (world_keys.go, as ur daemon
	// composes it), the config repo's heads live on evs, and the network's bootstrap is its first owner's follower's
	// own (§7.9, QD-824): what a world that serves gkm and evs at their origins runs. false keeps FakeGkm's deliveries
	// and the config heads in memory.
	RealKeys bool
	// With RealKeys, each device's gkm hint resync (SidecarSettings.Gkm.ResyncInterval), 0 keeping the sidecar's own
	// hourly one: 1 s, since gkm's hint writer resolves no sidecar id yet (plan-KEYS-hint-addressing) and the world's
	// exchange publishes no hint unless its settings turn sidecar push on, so a resync stands in for the hints. A world
	// of hosts whose gkm block resolves sidecar ids and whose exchange publishes hints keeps the sidecar's own
	// (sidecare2e's World).
	KeyResyncInterval time.Duration
	// With RealKeys, how long, in real time, a joining device's follower may take to stand, in place of SyncTimeout: its
	// join waits on a late delivery of the config repo's generation (§10.6), which a holder makes up to
	// LateDeliverJitter, 15 s, after gkm's hint, and which gkm refuses while its 5 s repo/ cache still holds the config
	// repo as keying. The joiner then asks again at its follower's first retry past LateAskInterval, a minute, which
	// the config client's backoff from 10 s puts 70 s or 150 s after its first follow (e2e-world saw 124 s). 5 min.
	KeyJoinTimeout time.Duration
	// Whether api's sequencer and rev/ store spend on a write budget of api's own over the world's Redis
	// (evsservice.NewWriteBudget on the world's host, as controller.installApiAuthorization builds api's), reporting its
	// spend beside every block's (ApiWriteBudget), rather than on the Net's fake budget: what a world of hosts runs
	// (sidecare2e's World). false keeps the Net's fake, whose violations a test reads.
	ApiWriteBudget bool
	// Whether each device's follower runs §16.6's spend check on the log's GrantActivates and bridge approvals
	// (ConfigLogSettings.AclSpendCheck, sidecar.NewSpendCheck) under FakeGkm, as ur daemon's follower does (cli's
	// daemon_config.go) and RealKeys' followers always do. false keeps FakeGkm's followers on the defaults, which
	// install none, so each refuses every GrantActivate as unbuilt while api, whose sequencer installs the check,
	// takes it.
	FollowerSpendCheck bool
}

func DefaultT3WorldSettings() *T3WorldSettings {
	gate := sidecar.DefaultAclGateSettings()
	gate.PollInterval = 0
	gate.PollJitter = 0
	gate.FailureBackoff = 0
	return &T3WorldSettings{
		Net:                     DefaultSettings(),
		Sidecar:                 DefaultT3SidecarSettings(),
		Exchange:                DefaultExchangeSettings(),
		Domain:                  "sidecart3.example",
		Gate:                    gate,
		GatedServices:           []string{"evs", "gkm"},
		SidecarYaml:             T3WorldPreviewSidecarYaml,
		SidecarLimits:           DefaultT3WorldSidecarLimits(),
		NetworkBalanceByteCount: 1024 * 1024 * 1024 * 1024,
		NetworkBalanceDuration:  24 * time.Hour,
		AuthKeyDuration:         10 * time.Minute,
		EnrollConcurrency:       4,
		StatementLife:           time.Hour,
		SyncTimeout:             60 * time.Second,
		KeyResyncInterval:       time.Second,
		KeyJoinTimeout:          5 * time.Minute,
		ApiWriteBudget:          false,
	}
}

// sidecar.yml with preview_networks alone: the networks a world admits to enrollment (D-TRUST-3).
func T3WorldPreviewSidecarYaml(previewNetworkIds []string) string {
	return fmt.Sprintf("sidecar:\n  preview_networks: [%s]\n", strings.Join(previewNetworkIds, ", "))
}

// pro.yml's sidecar limits a world runs under (§20.4), which model.Testing_SetEnforceSidecarLimits and
// Testing_SetSidecarLimits put on the process's parsed pro.yml for the world's life.
type T3WorldSidecarLimits struct {
	// enforce_sidecar_limits (§20.5): false keeps the limits dark, as production ships them, true refuses past them.
	Enforce bool
	// The tier whose sidecar: block a tier key sets: free, the tier model.Testing_CreateNetwork's networks are.
	Tier model.NetworkTier
	// Each limit by its pro.yml key, in the unit model.Pro reads it in: bytes for a size, milliseconds for a
	// duration, 0 or 1 for a switch, -1 for none. A tier key sets Tier's, a sidecar_abuse: key or
	// evs_unpaid_read_rate every tier's.
	LimitKeyValues map[string]int64
}

// §20.4's Community column and its sidecar_abuse: block, enforced: the free tier's numbers, so a scenario runs
// against the limits a network on it meets. §21.5's breaker rates stay unset, so the breaker never trips: no
// Postmaster sample reaches a world.
func DefaultT3WorldSidecarLimits() *T3WorldSidecarLimits {
	kib := int64(1024)
	mib := 1024 * kib
	gib := 1024 * mib
	return &T3WorldSidecarLimits{
		Enforce: true,
		Tier:    model.NetworkTierFree,
		LimitKeyValues: map[string]int64{
			"evs_unpaid_read_rate":        mib,
			"storage":                     gib,
			"sidecar_clients":             20,
			"top_level_client_limit":      100,
			"dns_entries":                 5,
			"tls_endpoints":               1,
			"config_admissions":           50,
			"config_statements_per_min":   60,
			"config_statements_burst":     600,
			"config_statements_per_day":   2000,
			"config_bases_per_min":        1,
			"config_bases_per_day":        200,
			"store_weight":                1,
			"evs_drops_per_day":           20,
			"acl_rules":                   50,
			"acl_groups":                  0,
			"acl_requests":                1,
			"acl_approvers_two":           0,
			"acl_network_shares":          0,
			"smtp_connections_per_day":    5,
			"smtp_bytes_per_day":          5 * mib,
			"smtp_recipients_per_message": 5,
			"smtp_recipients_per_day":     25,
			"smtp_message_bytes":          5 * mib,
			"smtp_burst":                  5,
			"smtp_burst_window":           int64(10 * time.Minute / time.Millisecond),
			"smtp_concurrent_connections": 1,
			"mail_inbound_message_bytes":  25 * mib,
			"mail_burners_active":         10,
			"mail_burner_creates_per_day": 3,
			"tls_endpoint_concurrent":     256,
			"tls_endpoint_new_per_s":      50,
			"tls_endpoint_mbit_per_s":     20,
			// sidecar_abuse:, every tier
			"mail_group_members":                    100,
			"mail_inbound_recipients":               100,
			"mail_inbound_repos_per_message":        8,
			"probation_dns_entries":                 5,
			"evs_closure_bytes_per_day":             64 * mib,
			"evs_closure_bytes_floor":               4 * mib,
			"evs_closure_uploads_per_s":             4,
			"evs_closure_uploads_per_min":           60,
			"evs_erasure_pushes_per_min":            60,
			"evs_drop_previews_per_day":             20,
			"evs_subscribers_per_repo":              1000,
			"evs_repos_per_client":                  500,
			"evs_read_receipts_per_repo_day":        256,
			"evs_read_receipt_bytes":                4 * kib,
			"dns_changes_per_hour":                  60,
			"dns_challenges_per_name_hour":          10,
			"dns_dkim_selectors":                    64,
			"tls_visitor_concurrent":                64,
			"tls_visitor_new_per_s":                 20,
			"tls_visitor_aggregate_multiple":        4,
			"smtp_community_connections_per_hour":   500,
			"smtp_community_median_multiple":        3,
			"smtp_destination_connections_per_hour": 200,
			"smtp_destination_concurrency":          10,
			"smtp_hard_bounce_percent":              10,
			"smtp_hard_bounce_floor":                10,
			"directory_requests_pending":            20,
			"directory_requests_per_day":            50,
			"directory_requests_per_run":            5,
			"directory_target_pending":              100,
			"directory_card_bytes":                  16 * kib,
		},
	}
}

// Puts limits on the process's parsed pro.yml until the returned pop.
func installT3WorldSidecarLimits(limits *T3WorldSidecarLimits) func() {
	popEnforce := model.Testing_SetEnforceSidecarLimits(limits.Enforce)
	popLimits := model.Testing_SetSidecarLimits(limits.Tier, limits.LimitKeyValues)
	return func() {
		popLimits()
		popEnforce()
	}
}

// The purpose a GroupForm is signed under (signed.proto's registry, §6.11).
const t3WorldGroupFormPurpose = "group-form"

// Signs a new network's owner credential, the network jwt AddNetwork hands its owner. Only a _test.go file installs
// one (Testing_SetT3WorldOwnerSigner), since the fixture signer it stands for, session's Testing_Sign, stays out of
// every non-test file (session's TestProductionSigningCannotUseFixtureBypass). nil: AddNetwork refuses.
var t3WorldOwnerSigner atomic.Pointer[func(credential *session.ByJwt) string]

// Installs sign as the signer of every new network's owner credential, until the returned pop: what a test binary that
// runs a T3World calls from a _test.go file (sidecart3's world_owner_sign_test.go, sidecare2e's). nil installs none.
func Testing_SetT3WorldOwnerSigner(sign func(credential *session.ByJwt) string) func() {
	var installed *func(credential *session.ByJwt) string
	if sign != nil {
		installed = &sign
	}
	previous := t3WorldOwnerSigner.Swap(installed)
	return func() {
		t3WorldOwnerSigner.Store(previous)
	}
}

// One gate cache of the world's, api's or a service's, and how many loads it has finished, which a sync waits on.
type t3WorldGate struct {
	name  string
	cache *sidecar.AclGateCache
	loads *sidecar.MonitorValue[uint64]
	close func()
}

// See the file's header.
type T3World struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *T3WorldSettings
	net      *sidecartest.Net
	host     *sidecar.ServiceHost
	// the verifier of the world's svc- key, which every host the world builds verifies tokens and rev/ under
	serviceTokens *oauth.ServiceTokenVerifier
	api           *T3Api
	exchange      *T3SidecarExchange
	gkm           *sidecartest.FakeGkm
	// api's own write budget with ApiWriteBudget, nil otherwise
	apiBudget *evsservice.WriteBudgetClient
	// api's host, its budget api's, and its first cluster, which each run of api's installs is over (startApi)
	apiHost  *sidecar.ServiceHost
	apiFirst sidecar.BlobCluster
	// api's current process state (startApi), nil once stopped
	apiLock sync.Mutex
	apiRun  *t3WorldApiRun

	stateLock sync.Mutex
	gates     []*t3WorldGate
	networks  []*T3Network
	// what Close undoes, newest first
	closes    []func()
	popConfig func()
	closed    bool
}

// A world on the private stores and the local stack's MinIO, inside a TestEnv that applies the migrations: see the
// file's header. Closed at t's end, or by Close. settings nil takes the defaults.
func NewT3World(t testing.TB, settings *T3WorldSettings) (world *T3World, returnErr error) {
	if settings == nil {
		settings = DefaultT3WorldSettings()
	}
	if !server.SidecarMg1Linked() || !server.SidecarMg2Linked() {
		return nil, errors.New("the world needs the log's tables, MG1 and MG2: build with -tags sidecar_pending")
	}
	sidecarSettings := settings.Sidecar
	if sidecarSettings == nil {
		sidecarSettings = DefaultT3SidecarSettings()
	}
	// every origin under the world's domain, or the exchange and DPoP refuse what the sidecars send
	if sidecarSettings.ApiOrigin != "https://api."+settings.Domain {
		return nil, fmt.Errorf("the api origin %q is not https://api.%s", sidecarSettings.ApiOrigin, settings.Domain)
	}
	for _, service := range sidecarSettings.ServiceNames {
		if origin := fmt.Sprintf(sidecarSettings.ServiceOriginFormat, service); origin != "https://"+service+"."+settings.Domain {
			return nil, fmt.Errorf("the origin of %s, %q, is not https://%s.%s", service, origin, service, settings.Domain)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	self := &T3World{ctx: ctx, cancel: cancel, settings: settings}
	t.Cleanup(self.Close)
	defer func() {
		if returnErr != nil {
			self.Close()
		}
	}()

	previousDomain, hadDomain := os.LookupEnv("WARP_DOMAIN")
	if err := os.Setenv("WARP_DOMAIN", settings.Domain); err != nil {
		return nil, err
	}
	self.closes = append(self.closes, func() {
		if hadDomain {
			os.Setenv("WARP_DOMAIN", previousDomain)
		} else {
			os.Unsetenv("WARP_DOMAIN")
		}
	})

	netSettings := settings.Net
	if netSettings == nil {
		netSettings = DefaultSettings()
	}
	self.net = sidecartest.NewNet(ctx, NetSettings(t, netSettings))
	self.closes = append(self.closes, self.net.Close)
	serviceTokens, pops, err := installT3WorldOauth(sidecarSettings.ApiOrigin)
	if err != nil {
		return nil, err
	}
	self.closes = append(self.closes, pops)
	self.serviceTokens = serviceTokens
	// the Net's stores on real time, since server's api, exchange and oauth judge every token and proof by it, its
	// Authenticator verifying under the world's own svc- key as a block given HostDeps.ServiceTokens does
	host := *self.net.Host()
	host.Clock = sidecar.RealClock{}
	host.Authenticator = sidecarhost.NewAuthenticatorWithVerifier(serviceTokens)
	self.host = &host
	// sidecar.yml from the start, before api's installs read its switches (the contract gate's) and so a host the
	// world serves reads its gates before the first network is added
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.gateNetworksWithLock()
	}()

	// pro.yml's sidecar limits for the world's life, before api takes a statement
	if settings.SidecarLimits != nil {
		self.closes = append(self.closes, installT3WorldSidecarLimits(settings.SidecarLimits))
	}
	first, err := host.Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	// what api's sequencer and rev/ store spend on: its own budget, as api's process builds one, or the Net's fake
	apiHost := *self.host
	if settings.ApiWriteBudget {
		budget, err := evsservice.NewWriteBudget(ctx, &apiHost, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("api's write budget: %w", err)
		}
		self.closes = append(self.closes, budget.Close)
		apiHost.Budget = budget
		self.apiBudget = budget
	}
	self.apiHost = &apiHost
	self.apiFirst = first
	// api's log and exchange, as its process installs them over its first cluster
	apiRun, err := self.startApi()
	if err != nil {
		return nil, err
	}
	self.apiRun = apiRun
	self.closes = append(self.closes, self.stopApi)

	self.api = NewT3Api(ctx, self.net, sidecarSettings)
	exchange, err := NewT3SidecarExchange(ctx, t, settings.Exchange)
	if err != nil {
		return nil, err
	}
	self.exchange = exchange
	self.closes = append(self.closes, exchange.Close)
	self.gkm = sidecartest.NewFakeGkm(host.Clock, host.Blobs, nil)
	return self, nil
}

// What api's process installs over its gate cache beside the sequencer and the exchange's scopes
// (controller.installApiAuthorization, installApiProjection), on the world's host and its first cluster, until the
// returned pop: the approval gate's states, the dns scopes' labels, the smtp routes' §8 checks, the edge logins' source,
// rev/'s store signing under the world's svc- key, the issuer's worker that claims its tasks (revocations, due ticks and
// their hooks), and the contract gate when sidecar.yml's acl_contract_gate is on now, as the install reads the switch
// at api's start.
func installT3WorldApi(ctx context.Context, host *sidecar.ServiceHost, first sidecar.BlobCluster, cache *sidecar.AclGateCache) func() {
	pops := []func(){}
	pops = append(pops, model.SetApprovalGateStates(model.ApprovalGateStatesOf(cache)))
	pops = append(pops, model.SetDnsScopeLabelSource(model.DnsScopeLabelSourceOf(cache)))
	pops = append(pops, model.SetSmtpApiGate(model.SmtpApiGateOf(cache)))
	oauth.SetEdgeLoginSource(oauth.NewProjectionEdgeLoginSource(cache))
	pops = append(pops, func() { oauth.SetEdgeLoginSource(nil) })
	revSettings := model.DefaultSidecarRevSettings()
	revSettings.Clock = host.Clock
	revStore := model.NewSidecarRevStore(first, oauth.SignRevocation, revSettings)
	if host.Budget != nil {
		prices := evsservice.DefaultPriceTable4KiB()
		revStore.SetWriteBudget(host.Budget, func(byteCount int64) int64 {
			price, _ := prices.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationCreate, ObjectByteCount: byteCount, Count: 1})
			return price
		})
	}
	model.SetSidecarRevStore(revStore)
	pops = append(pops, func() { model.SetSidecarRevStore(nil) })
	if server.CurrentSidecarConfig().AclContractGate() {
		gate := controller.NewAclContractGate(ctx, cache, nil)
		restore := controller.SetAclContractGate(gate)
		pops = append(pops, func() {
			restore()
			gate.Close()
		})
	}
	workerSettings := task.DefaultTaskWorkerSettings()
	workerSettings.ClaimRegisteredTargetsOnly = true
	workerCtx, stopWorker := context.WithCancel(ctx)
	worker := task.NewTaskWorker(workerCtx, workerSettings)
	worker.AddTargets(model.AuthorizationIssuerTargets()...)
	var lifetime sync.WaitGroup
	lifetime.Go(func() {
		// as api's own supervision: an error ends a pass, and the next starts after the worker's retry wait
		for workerCtx.Err() == nil {
			if server.HandleError(worker.Run) == nil {
				return
			}
			select {
			case <-workerCtx.Done():
				return
			case <-time.After(workerSettings.RetryTimeoutAfterError):
			}
		}
	})
	pops = append(pops, func() {
		stopWorker()
		worker.Drain()
		worker.Close()
		lifetime.Wait()
	})
	return func() {
		for index := len(pops) - 1; 0 <= index; index -= 1 {
			pops[index]()
		}
	}
}

// The world's issuer at origin and its two signer keys, throwaways made here, until the returned pop: the svc- key,
// which api's process signs service tokens and rev/ objects with, and the access tokens' key, which the issuer signs
// a resource's tokens with (mcp's among them); and the verifier holding the svc- key, as a block's mounted jwks gives
// one, which the world hands each host it builds (HostDeps.ServiceTokens' way), so no host reads the process's.
func installT3WorldOauth(origin string) (*oauth.ServiceTokenVerifier, func(), error) {
	signerKey := func(set oauth.SignerKeySet) (*oauth.SignerKey, error) {
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
		if err != nil {
			return nil, err
		}
		return &oauth.SignerKey{Kid: set.KidPrefix() + thumbprint, Alg: "ES256", Set: set, PrivateKey: privateKey}, nil
	}
	key, err := signerKey(oauth.SignerKeySetService)
	if err != nil {
		return nil, nil, err
	}
	accessKey, err := signerKey(oauth.SignerKeySetMcp)
	if err != nil {
		return nil, nil, err
	}
	pops := []func(){oauth.Testing_SetIssuer(origin), oauth.Testing_SetSignerKeys(key, accessKey)}
	popAll := func() {
		for index := len(pops) - 1; 0 <= index; index -= 1 {
			pops[index]()
		}
	}
	verifier, err := oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
		Issuer: origin,
		Keys:   oauth.PublicJwks(oauth.SignerKeySetService).Keys,
	})
	if err != nil {
		popAll()
		return nil, nil, err
	}
	return verifier, popAll, nil
}

// A gate cache over the world's host named name, watching the log's {acl_}v events, which every sync waits on until
// its owner removes it or the world closes. Its close is safe to call again, and the world's Close calls it in its
// turn.
func (self *T3World) addGate(name string) (*t3WorldGate, error) {
	gateSettings := *self.settings.Gate
	gate := &t3WorldGate{name: name, loads: sidecar.NewMonitorValue(uint64(0))}
	gateSettings.OnLoad = func(networkId sidecar.Id, pin *bridge.AuthPin, err error) {
		gate.loads.Update(func(count uint64) uint64 { return count + 1 })
	}
	cache, closeCache, err := sidecarhost.WatchAclGate(self.ctx, self.host, nil, &gateSettings)
	if err != nil {
		return nil, fmt.Errorf("%s's gate cache: %w", name, err)
	}
	gate.cache = cache
	gate.close = sync.OnceFunc(closeCache)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		gate.close()
		return nil, errors.New("the world has closed")
	}
	self.gates = append(self.gates, gate)
	self.closes = append(self.closes, gate.close)
	return gate, nil
}

// Stops waiting on gate, which its owner closes.
func (self *T3World) removeGate(gate *t3WorldGate) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.gates = slices.DeleteFunc(self.gates, func(held *t3WorldGate) bool { return held == gate })
}

// A gate cache named name over the world's host, as a block's own follows the log, which every sync waits on
// (SyncGates) until the returned close, which also ends the cache: what a host the world serves reads its
// authorization state from, so a statement's filer knows the host holds it before it acts. Its close is safe to call
// again, and the world's Close calls it in its turn.
func (self *T3World) AddGate(name string) (*sidecar.AclGateCache, func(), error) {
	gate, err := self.addGate(name)
	if err != nil {
		return nil, nil, err
	}
	return gate.cache, func() {
		self.removeGate(gate)
		gate.close()
	}, nil
}

// api's own write budget, whose process reports its spend on every metadata cluster beside the blocks': nil unless
// ApiWriteBudget, when api spends on the Net's fake budget.
func (self *T3World) ApiWriteBudget() *evsservice.WriteBudgetClient {
	return self.apiBudget
}

// The verifier of the world's svc- key, which every host the world serves verifies service tokens and rev/ under.
func (self *T3World) ServiceTokens() *oauth.ServiceTokenVerifier {
	return self.serviceTokens
}

// Server's exchange the world's sidecars connect on.
func (self *T3World) Exchange() *T3SidecarExchange {
	return self.exchange
}

// The world's context, which ends at its Close.
func (self *T3World) Context() context.Context {
	return self.ctx
}

// The platform the world's sidecars open their clients on, T3Api for their requests and server's exchange for their
// connect clients: what an edge block's client pool opens its urnetwork-edge clients on too.
func (self *T3World) Platform() sidecar.SidecarPlatform {
	return &t3SidecarPlatform{api: self.api, exchange: self.exchange.platform}
}

// Runs close at the world's Close, before what the world made itself is ended, newest first; at once when the world
// has closed.
func (self *T3World) OnClose(close func()) {
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.closes = append(self.closes, close)
		return true
	}()
	if !kept {
		close()
	}
}

// The Net whose stores, capture and fakes the world's are.
func (self *T3World) Net() *sidecartest.Net {
	return self.net
}

// The Net's host on real time, which the world's services run on.
func (self *T3World) Host() *sidecar.ServiceHost {
	return self.host
}

// Server's api in process, the service origins' lb.
func (self *T3World) Api() *T3Api {
	return self.api
}

// The world's settings.
func (self *T3World) Settings() *T3WorldSettings {
	return self.settings
}

// The networks added so far, in order.
func (self *T3World) Networks() []*T3Network {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.networks)
}

// Ends the world: its networks' sidecars, then the services, gates and installs it made, newest first. Safe to call
// again.
func (self *T3World) Close() {
	var networks []*T3Network
	var closes []func()
	var popConfig func()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return
		}
		self.closed = true
		networks, closes, popConfig = self.networks, self.closes, self.popConfig
		self.networks, self.closes, self.gates, self.popConfig = nil, nil, nil, nil
	}()
	for _, network := range networks {
		network.close()
	}
	if popConfig != nil {
		popConfig()
	}
	for index := len(closes) - 1; 0 <= index; index -= 1 {
		closes[index]()
	}
	self.cancel()
}

// Closes every network's sidecars now, each network's newest first, as Close does before it ends the services: what a
// world of hosts runs before it stops them, so no host drains under a sidecar's retries. Safe to call again, and Close
// calls it in its turn.
func (self *T3World) CloseNetworks() {
	for _, network := range self.Networks() {
		network.close()
	}
}

// Waits until every gate cache of the world holds networkId at version or past it, asking each to load: what a
// statement's filer waits on before a device acts on it at a gate, as api's {acl_}v event brings a block's gate
// within 5 s (§8.9). SyncTimeout bounds it.
func (self *T3World) SyncGates(ctx context.Context, networkId server.Id, version uint64) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.SyncTimeout)
	defer cancel()
	self.stateLock.Lock()
	gates := slices.Clone(self.gates)
	self.stateLock.Unlock()
	for _, gate := range gates {
		for {
			// armed before the read, so a load between them wakes the wait
			_, loaded := gate.loads.Get()
			snapshot, status := gate.cache.State(sidecar.Id(networkId))
			if status == sidecar.AclGateLoaded && snapshot != nil && version <= snapshot.Pin.GetVersion() {
				break
			}
			gate.cache.Changed(sidecar.Id(networkId))
			select {
			case <-loaded:
			case <-ctx.Done():
				held := uint64(0)
				if snapshot != nil {
					held = snapshot.Pin.GetVersion()
				}
				return fmt.Errorf("%s's gate holds network %s at %d (%v), not %d: %w", gate.name, networkId, held, status, version, ctx.Err())
			}
		}
	}
	return nil
}

// Puts sidecar.yml's preview_networks at every network of the world, as an operator lists the networks a preview
// admits, and opens each gated service's gate on the Net's sidecar.yml to them.
func (self *T3World) gateNetworksWithLock() {
	networkIds := []string{}
	connectIds := []connect.Id{}
	for _, network := range self.networks {
		networkIds = append(networkIds, strconv.Quote(network.NetworkId.String()))
		connectIds = append(connectIds, connect.Id(network.NetworkId))
	}
	if self.popConfig != nil {
		self.popConfig()
	}
	sidecarYaml := self.settings.SidecarYaml
	if sidecarYaml == nil {
		sidecarYaml = T3WorldPreviewSidecarYaml
	}
	self.popConfig = server.Testing_PushSidecarConfig(sidecarYaml(networkIds))
	for _, service := range self.settings.GatedServices {
		self.net.Fakes().Gates.OpenService(service, connectIds...)
	}
}

// A personal network of a new owner, named name: in preview_networks, in every gated service's gate and funded in the
// Net's quota, with no sidecar yet. Refused, with nothing made, until a _test.go file installs the owner's signer
// (Testing_SetT3WorldOwnerSigner).
func (self *T3World) AddNetwork(ctx context.Context, name string) (*T3Network, error) {
	sign := t3WorldOwnerSigner.Load()
	if sign == nil {
		return nil, errors.New("no signer of the owner's credential: a _test.go file installs one with Testing_SetT3WorldOwnerSigner")
	}
	network := &T3Network{
		world:        self,
		Name:         name,
		NetworkId:    server.NewId(),
		OwnerUserId:  server.NewId(),
		configRepoId: sidecar.NewEvsRepoId(),
	}
	networkName := fmt.Sprintf("t3%s%s", strings.ReplaceAll(network.NetworkId.String(), "-", "")[:12], strings.ToLower(name))
	model.Testing_CreateNetwork(ctx, network.NetworkId, networkName, network.OwnerUserId)
	network.root = "Bearer " + (*sign)(session.NewByJwt(network.NetworkId, network.OwnerUserId, networkName, false, false))
	network.heads = sidecartest.NewFakeConfigHeads(network.configRepoId)
	self.net.Fakes().Quota.SetBalance(connect.Id(network.NetworkId), self.settings.NetworkBalanceByteCount)
	// and in server's balances, which a block's Quota reads through api's internal quota surface
	now := server.NowUtc()
	if err := model.AddBasicTransferBalance(ctx, network.NetworkId, model.ByteCount(self.settings.NetworkBalanceByteCount), now, now.Add(self.settings.NetworkBalanceDuration)); err != nil {
		return nil, fmt.Errorf("%s's transfer balance: %w", name, err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil, errors.New("the world has closed")
	}
	self.networks = append(self.networks, network)
	self.gateNetworksWithLock()
	return network, nil
}

// One network of a world: its owner, its sidecars, its log's config heads and genesis.
type T3Network struct {
	world        *T3World
	Name         string
	NetworkId    server.Id
	OwnerUserId  server.Id
	configRepoId sidecar.Id
	// the owner's own credential, a network jwt as a signed-in owner holds
	root  string
	heads *sidecartest.FakeConfigHeads
	// a team's first Owner's member_id (AddNetworkOf), zero in a personal network, whose owner member is its user
	ownerMemberId sidecar.Id

	stateLock sync.Mutex
	devices   []*T3Device
	owner     *T3Device
	genesis   *[sha256.Size]byte
	// the devices Sync no longer waits on (Retire)
	retired map[*T3Device]bool
	// each device's retirement as a Sync waiting on it reads it, closed once Retire takes it out of the syncs
	retirements map[*T3Device]chan struct{}
	// a test's look at each wait of Sync's, called just before it waits on device; nil for none
	syncWaiting func(device *T3Device)
}

// What a Sync waiting on device reads: the channel Retire closes, closed already for a retired device, and the test's
// look at the wait.
func (self *T3Network) syncRetirement(device *T3Device) (<-chan struct{}, func(device *T3Device)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.retirementWithLock(device), self.syncWaiting
}

// device's retirement channel, made at the first ask: closed once Retire took it out of the syncs.
func (self *T3Network) retirementWithLock(device *T3Device) chan struct{} {
	if self.retirements == nil {
		self.retirements = map[*T3Device]chan struct{}{}
	}
	retirement, ok := self.retirements[device]
	if !ok {
		retirement = make(chan struct{})
		if self.retired[device] {
			close(retirement)
		}
		self.retirements[device] = retirement
	}
	return retirement
}

// The network's owner member: its owner user in a personal network (§7.5), a team's first Owner's member_id.
func (self *T3Network) MemberId() sidecar.Id {
	if self.ownerMemberId != (sidecar.Id{}) {
		return self.ownerMemberId
	}
	return sidecar.Id(self.OwnerUserId)
}

// Every sidecar added, in order.
func (self *T3Network) Devices() []*T3Device {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.devices)
}

// The device that bootstrapped the network, nil before.
func (self *T3Network) Owner() *T3Device {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.owner
}

// Closes every device, newest first.
func (self *T3Network) close() {
	devices := self.Devices()
	for index := len(devices) - 1; 0 <= index; index -= 1 {
		devices[index].close()
	}
}

// A one-use auth key the owner makes through POST /network/auth-keys, as `ur network auth-key` does.
func (self *T3Network) authKey(ctx context.Context) (string, error) {
	return self.authKeyAs(ctx, self.root, nil)
}

// A one-use auth key made through POST /network/auth-keys with root, a person's network credential, carrying tags (tag
// ids, 32 lowercase hex each; none for an untagged key).
func (self *T3Network) authKeyAs(ctx context.Context, root string, tags []string) (string, error) {
	status, answer, err := self.world.api.Call(ctx, http.MethodPost, "/network/auth-keys", root, &model.CreateAuthKeyArgs{
		DurationSeconds: int64(self.world.settings.AuthKeyDuration / time.Second),
		MaxUses:         1,
		Tags:            tags,
	})
	if err != nil {
		return "", err
	}
	result := &model.CreateAuthKeyResult{}
	if status != http.StatusOK || json.Unmarshal(answer, result) != nil || result.AuthKey == "" {
		message := ""
		if result.Error != nil {
			message = result.Error.Message
		}
		return "", fmt.Errorf("POST /network/auth-keys: %d %s", status, message)
	}
	return result.AuthKey, nil
}

// count sidecars, each enrolled through api from an auth key of its own and Online on server's exchange, at most
// EnrollConcurrency at once, named for the network and their order. settings, called for each, adjusts a sidecar's
// settings past the defaults; nil for none. A failure closes what it enrolled and answers every error. One call at a
// time per network, so the names stay apart.
func (self *T3Network) AddSidecars(ctx context.Context, count int, settings func(name string, sidecarSettings *sidecar.SidecarSettings)) ([]*T3Device, error) {
	return self.AddSidecarsWith(ctx, &T3EnrollArgs{Count: count, Settings: settings})
}

// How AddSidecarsWith enrolls: whose credential makes each one-use auth key, the member its devices are, and the
// key's tags.
type T3EnrollArgs struct {
	// "Bearer <jwt>", the person's network credential each auth key is made with: "" for the network's owner's.
	Root string
	// The member the devices are, whose credential Root is: zero for the network's owner member. Unread for tagged
	// devices, which are no member's.
	MemberId sidecar.Id
	// Tag ids on each auth key, 32 lowercase hex each (§7.8): tagged devices. None for untagged ones.
	Tags []string
	// How many devices.
	Count int
	// Called for each, adjusting its settings past the defaults; nil for none.
	Settings func(name string, sidecarSettings *sidecar.SidecarSettings)
	// What the devices' names start with, each then -<index> from 0 within the call, so calls naming different
	// prefixes may run at once on one network. "": the network's name, each numbered from the network's count, one call
	// at a time.
	Name string
}

// AddSidecars under args: each device enrolled from an auth key args' credential makes, carrying args' tags, so a
// team's member enrolls its own devices and a tag owner its tagged ones. One call at a time per network, unless each
// names a prefix of its own (T3EnrollArgs.Name).
func (self *T3Network) AddSidecarsWith(ctx context.Context, args *T3EnrollArgs) ([]*T3Device, error) {
	count, settings := args.Count, args.Settings
	root := args.Root
	if root == "" {
		root = self.root
	}
	memberId := args.MemberId
	if memberId == (sidecar.Id{}) && args.Root == "" {
		memberId = self.MemberId()
	}
	if 0 < len(args.Tags) {
		memberId = sidecar.Id{}
	}
	prefix, start := self.Name, len(self.Devices())
	if args.Name != "" {
		prefix, start = args.Name, 0
	}
	added := make([]*T3Device, count)
	errs := make([]error, count)
	concurrency := max(1, self.world.settings.EnrollConcurrency)
	tickets := make(chan struct{}, concurrency)
	var group sync.WaitGroup
	for index := range count {
		group.Go(func() {
			tickets <- struct{}{}
			defer func() { <-tickets }()
			name := fmt.Sprintf("%s-%d", prefix, start+index)
			added[index], errs[index] = self.enroll(ctx, name, root, memberId, args.Tags, settings)
		})
	}
	group.Wait()
	if err := errors.Join(errs...); err != nil {
		for _, device := range added {
			if device != nil {
				device.close()
			}
		}
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.devices = append(self.devices, added...)
	return added, nil
}

// One sidecar named name enrolled from a fresh auth key root makes with tags, Online, the device of memberId (zero
// for a tagged one). It runs until the world closes, ctx bounding only its enrollment.
func (self *T3Network) enroll(ctx context.Context, name string, root string, memberId sidecar.Id, tags []string, settings func(name string, sidecarSettings *sidecar.SidecarSettings)) (*T3Device, error) {
	authKey, err := self.authKeyAs(ctx, root, tags)
	if err != nil {
		return nil, err
	}
	sidecarSettings := sidecar.DefaultSidecarSettings()
	if self.world.settings.RealKeys && 0 < self.world.settings.KeyResyncInterval {
		sidecarSettings.Gkm.ResyncInterval = self.world.settings.KeyResyncInterval
	}
	if settings != nil {
		settings(name, sidecarSettings)
	}
	running, err := StartT3Sidecar(self.world.ctx, self.world.net, name, self.world.api, self.world.exchange, sidecarSettings)
	if err != nil {
		return nil, err
	}
	if err := running.EnrollOnline(ctx, &sidecar.SidecarEnrollCredential{AuthKey: authKey}); err != nil {
		running.Close()
		return nil, err
	}
	identity := running.Sidecar().Identity()
	if identity == nil || server.Id(identity.NetworkId) != self.NetworkId {
		running.Close()
		return nil, fmt.Errorf("%s enrolled in no network of %s's", name, self.Name)
	}
	device := &T3Device{T3Sidecar: running, Network: self, MemberId: memberId, TagIds: slices.Clone(tags)}
	if err := device.buildKeys(); err != nil {
		running.Close()
		return nil, err
	}
	return device, nil
}

// The device's key client on its current Sidecar (the file's keys row): its real one with RealKeys, else FakeGkm's
// for its identity. At enrollment, and again at each Restart, whose new Sidecar the old clients no longer reach.
func (self *T3Device) buildKeys() error {
	running := self.T3Sidecar
	network := self.Network
	identity := running.Sidecar().Identity()
	if identity == nil {
		return fmt.Errorf("%s holds no identity", self.Name)
	}
	device := self
	if network.world.settings.RealKeys {
		keys, err := newT3WorldKeys(running.Sidecar(), running.Settings.Log, func(ctx context.Context, signedType bridge.SignedType, body proto.Message) error {
			_, err := network.FileStatement(ctx, device, signedType, body)
			return err
		})
		if err != nil {
			return err
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.keys = keys
		self.Gkm = keys.gkm
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.Gkm = &t3WorldGkmClient{
		GkmClient: network.world.gkm.Client(identity.SidecarId, func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
			return running.Sidecar().Sign(purpose, signedType, body, agentId)
		}),
		device: device,
	}
	return nil
}

// FakeGkm's client for one device (the file's keys row), which also stores the GroupForm gkm's KF1 stores (gkm-1, §10.6),
// signed by the device's identity key at its follower's pin, at gkm/repo/<repo_id>/<seq>: evs's activate route reads the
// newest of them for the group whose gen/1 it then looks for (evsservice's repo.go), and FakeGkm stores no form.
type t3WorldGkmClient struct {
	sidecar.GkmClient
	device *T3Device
}

// KF1: FakeGkm's group and generation 1, then the repo's first GroupForm naming that group.
func (self *t3WorldGkmClient) CreateGroup(ctx context.Context, args *sidecar.GkmCreateGroupArgs) (*sidecar.EvsRepoKey, error) {
	key, err := self.GkmClient.CreateGroup(ctx, args)
	if err != nil {
		return nil, err
	}
	world := self.device.Network.world
	groupId, _, ok := world.gkm.Newest(args.RepoId)
	if !ok {
		return nil, fmt.Errorf("the fake gkm holds no group of repo %s after its KF1", args.RepoId)
	}
	follower := self.device.Follower()
	if follower == nil || follower.Client() == nil {
		return nil, fmt.Errorf("%s forms a group by a follower that stands", self.device.Name)
	}
	formBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.GroupForm{
		RepoId:     args.RepoId.Bytes(),
		GroupId:    groupId.Bytes(),
		Generation: 1,
		Auth:       proto.Clone(follower.Client().Pin()).(*bridge.AuthPin),
	})
	if err != nil {
		return nil, err
	}
	signed, err := self.device.Sidecar().Sign(t3WorldGroupFormPurpose, bridge.SignedType_SIGNED_GROUP_FORM, formBytes, nil)
	if err != nil {
		return nil, err
	}
	signedBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	if err != nil {
		return nil, err
	}
	cluster, err := world.host.Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	if _, err := sidecar.PutImmutable(ctx, cluster, gkmservice.GkmFormKey(args.RepoId, 1), signedBytes, nil); err != nil {
		return nil, fmt.Errorf("repo %s's GroupForm: %w", args.RepoId, err)
	}
	return key, nil
}

// The genesis's A_0 once Bootstrap made it.
func (self *T3Network) genesisChain() (*[sha256.Size]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.genesis == nil {
		return nil, fmt.Errorf("%s has no genesis yet", self.Name)
	}
	return self.genesis, nil
}

// owner, an enrolled sidecar of the network, signs the Genesis and base 1 through POST /network/config/projection
// (§7.9's bootstrap), its first head on the network's config heads, then follows the log with its key client: base 1
// held by the owner's follower and by every gate of the world.
func (self *T3Network) Bootstrap(ctx context.Context, owner *T3Device) error {
	if owner.Network != self {
		return fmt.Errorf("%s is no sidecar of %s", owner.Name, self.Name)
	}
	if owner.keys != nil {
		return self.bootstrapByFollower(ctx, owner)
	}
	running := owner.Sidecar()
	identity := running.Identity()
	sign := func(purpose string, signedType bridge.SignedType, body []byte) (*bridge.Signed, error) {
		return running.Sign(purpose, signedType, body, nil)
	}
	routes := &t3NetworkRoutes{api: self.world.api, bearer: owner.Bearer()}
	bootstrap, err := sidecar.BootstrapNetwork(ctx, &sidecar.ConfigBootstrapArgs{
		NetworkId:         identity.NetworkId,
		OwnerMemberId:     self.MemberId(),
		SidecarId:         identity.SidecarId,
		IdentityPublicKey: identity.IdentityPublicKey,
		Sign:              sign,
		ConfigRepoId:      self.configRepoId,
		Heads:             self.heads.ForSigner(identity.SidecarId, sign),
		Publisher:         routes,
		Projection:        routes,
		Clock:             sidecar.RealClock{},
	})
	if err != nil {
		return fmt.Errorf("%s's bootstrap through api: %w", self.Name, err)
	}
	if !bootstrap.Won || bootstrap.Pin.GetVersion() != 1 {
		return fmt.Errorf("%s's genesis did not take entry 1: won %t at %d", self.Name, bootstrap.Won, bootstrap.Pin.GetVersion())
	}
	genesis := bootstrap.GenesisChain
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.owner = owner
		self.genesis = &genesis
	}()
	if err := owner.follow(ctx, genesis); err != nil {
		return err
	}
	return self.Sync(ctx, bootstrap.Pin.GetVersion())
}

// With RealKeys, owner's follower bootstraps the network itself, as ur daemon's first owner does (§7.9, QD-824): its
// draft, KF1's draft of the config group at gkm, the first head's push sealed on evs, base 1 through api and the config
// repo's activation, until it stands; the genesis A_0 it then anchors at is the network's.
func (self *T3Network) bootstrapByFollower(ctx context.Context, owner *T3Device) error {
	follower, err := owner.Sidecar().FollowConfig(&sidecar.SidecarConfigFollowerArgs{
		BootstrapFirstOwner: true,
		Heads:               owner.keys.heads,
		Gkm:                 owner.keys.gkm,
		Settings:            t3WorldKeysFollowerSettings(),
	})
	if err != nil {
		return fmt.Errorf("%s's follower: %w", owner.Name, err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, self.world.settings.SyncTimeout)
	defer cancel()
	if _, err := follower.WaitStanding(waitCtx); err != nil {
		status := follower.Status()
		follower.Close()
		return fmt.Errorf("%s's follower did not bootstrap %s (%s: %s): %w", owner.Name, self.Name, status.State, status.Reason, err)
	}
	anchor, err := owner.Sidecar().Anchor()
	if err != nil || len(anchor.GenesisSha256) != sha256.Size {
		follower.Close()
		return fmt.Errorf("%s anchors at no genesis after its bootstrap: %v", owner.Name, err)
	}
	// the bootstrap formed the config group at gkm (KF1 after base 1), whose generation 1 the owner holds
	state := follower.VerifiedState()
	configRepoId, err := sidecar.IdFromBytes(state.Genesis().GetConfigRepoId())
	if err != nil {
		follower.Close()
		return fmt.Errorf("%s's genesis names no config repo: %w", self.Name, err)
	}
	if _, err := owner.keys.gkm.CurrentRepoKey(waitCtx, configRepoId); err != nil {
		follower.Close()
		return fmt.Errorf("%s's bootstrap stood holding no key of the config repo's group: %w", owner.Name, err)
	}
	genesis := [sha256.Size]byte(anchor.GenesisSha256)
	func() {
		owner.stateLock.Lock()
		defer owner.stateLock.Unlock()
		owner.follower = follower
	}()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.owner = owner
		self.genesis = &genesis
	}()
	return self.Sync(ctx, follower.Client().Pin().GetVersion())
}

// The owner admits each device, a new device of its member, as `ur approve` does on its approval item (§5.6, §7.9's
// SidecarAdmit by the member's consented device); then each trusts the genesis and follows the log with its key
// client, standing at the admissions. Each admission is one act, in order.
func (self *T3Network) Admit(ctx context.Context, devices []*T3Device) error {
	owner := self.Owner()
	genesis, err := self.genesisChain()
	if err != nil || owner == nil {
		return fmt.Errorf("%s admits nothing before its bootstrap: %w", self.Name, err)
	}
	for _, device := range devices {
		identity := device.Sidecar().Identity()
		memberId := device.MemberId
		if memberId == (sidecar.Id{}) {
			memberId = self.MemberId()
		}
		approvalId, err := owner.Sidecar().OpenAdmission(&sidecar.SidecarAdmissionRequest{
			SidecarId:         identity.SidecarId,
			MemberId:          memberId,
			IdentityPublicKey: identity.IdentityPublicKey,
		})
		if err != nil {
			return fmt.Errorf("%s's admission item: %w", device.Name, err)
		}
		if err := owner.Sidecar().DecideApproval(ctx, approvalId, sidecar.SidecarDecisionApprove, sidecar.SidecarActConfirmed, nil); err != nil {
			return fmt.Errorf("%s's admission: %w", device.Name, err)
		}
	}
	client := owner.Follower().Client()
	if client == nil {
		return fmt.Errorf("%s's follower no longer stands", owner.Name)
	}
	if err := client.CatchUp(ctx); err != nil {
		return err
	}
	tip := client.Pin().GetVersion()
	for _, device := range devices {
		if err := device.follow(ctx, *genesis); err != nil {
			return err
		}
	}
	return self.Sync(ctx, tip)
}

// Waits until every gate of the world and every following device of the network hold its log at version or past
// it, so each key client holds the state there (SidecarConfigFollowerArgs.Gkm). A retired device is not waited on,
// and a wait on a device ends once Retire takes it out of the syncs: a member removed while another statement's sync
// waits on its device follows nothing more, since api's denial deactivates its client at once.
func (self *T3Network) Sync(ctx context.Context, version uint64) error {
	if err := self.world.SyncGates(ctx, self.NetworkId, version); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, self.world.settings.SyncTimeout)
	defer cancel()
	for _, device := range self.Devices() {
		follower := device.Follower()
		if follower == nil {
			continue
		}
		retirement, waiting := self.syncRetirement(device)
		select {
		case <-retirement:
			continue
		default:
		}
		if waiting != nil {
			waiting(device)
		}
		err := func() error {
			waitCtx, waitCancel := context.WithCancel(ctx)
			defer waitCancel()
			go func() {
				select {
				case <-retirement:
					waitCancel()
				case <-waitCtx.Done():
				}
			}()
			_, err := follower.Client().WaitFor(waitCtx, version)
			return err
		}()
		if err != nil {
			select {
			case <-retirement:
				// taken out of the syncs while this one waited on it
				continue
			default:
			}
			return fmt.Errorf("%s did not follow %s's log to %d: %w", device.Name, self.Name, version, err)
		}
	}
	return nil
}

// Files one of §7.9's or §8.8's statements as device: body, whose header is set here at device's verified pin with a
// fresh nonce and a not_after StatementLife from now, signed by device's identity key and posted to POST
// /network/config/delta with its token, as its act runner posts one (CORE-M2-02); then synced (Sync) at the entry's
// pin, which it answers. A refusal answers api's status and message.
func (self *T3Network) FileStatement(ctx context.Context, device *T3Device, signedType bridge.SignedType, body proto.Message) (*bridge.AuthPin, error) {
	follower := device.Follower()
	if follower == nil || follower.Client() == nil {
		return nil, fmt.Errorf("%s files by a follower that stands", device.Name)
	}
	purpose, ok := sidecar.ConfigStatementPurpose(signedType)
	if !ok {
		return nil, fmt.Errorf("%s is no statement of the log", signedType)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := &bridge.StatementHeader{
		NetworkId:  sidecar.Id(self.NetworkId).Bytes(),
		At:         proto.Clone(follower.Client().Pin()).(*bridge.AuthPin),
		Nonce:      nonce,
		NotAfterMs: self.world.host.ClockOrReal().Now().Add(self.world.settings.StatementLife).UnixMilli(),
	}
	statement := proto.Clone(body)
	if err := setStatementHeader(statement, header); err != nil {
		return nil, err
	}
	statementBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(statement)
	if err != nil {
		return nil, err
	}
	signed, err := device.Sidecar().Sign(purpose, signedType, statementBytes, nil)
	if err != nil {
		return nil, err
	}
	signedBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	if err != nil {
		return nil, err
	}
	status, answer, err := self.world.api.Call(ctx, http.MethodPost, "/network/config/delta", device.Bearer(), &controller.NetworkConfigDeltaArgs{Signed: signedBytes})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s's %s: POST /network/config/delta: %d %s", device.Name, signedType, status, strings.TrimSpace(string(answer)))
	}
	result := &controller.NetworkConfigPinResult{}
	if err := json.Unmarshal(answer, result); err != nil || result.N == 0 {
		return nil, fmt.Errorf("%s's %s: POST /network/config/delta answered no pin", device.Name, signedType)
	}
	pin := &bridge.AuthPin{Version: result.N, Chain: result.Chain}
	return pin, self.Sync(ctx, pin.GetVersion())
}

// Sets a statement's StatementHeader, its field named header, which every statement of the log carries.
func setStatementHeader(statement proto.Message, header *bridge.StatementHeader) error {
	message := statement.ProtoReflect()
	field := message.Descriptor().Fields().ByName("header")
	if field == nil || field.Message() == nil || field.Message().FullName() != header.ProtoReflect().Descriptor().FullName() {
		return fmt.Errorf("%s carries no StatementHeader", message.Descriptor().FullName())
	}
	message.Set(field, protoreflect.ValueOfMessage(header.ProtoReflect()))
	return nil
}

// creator, a device whose follower stands, creates a repo of kind through evs as `ur evs create` does (§11.12): its
// RepoCreate filed through api (FileStatement), placement, KF1 by its key client and activation at the evs the world
// serves. The repo, on the creator's EvsClient.
func (self *T3Network) CreateRepo(ctx context.Context, creator *T3Device, kind protocol.RepoKind) (*sidecar.EvsRepo, error) {
	client, err := creator.EvsClient()
	if err != nil {
		return nil, err
	}
	repo, err := client.CreateRepo(ctx, &sidecar.EvsCreateRepoArgs{Kind: kind, Confirmed: true})
	if err != nil {
		return nil, fmt.Errorf("%s's repo of %s: %w", creator.Name, kind, err)
	}
	return repo, nil
}

// device, a config writer of the network whose follower stands, saves source as the network's ACL document, as
// `ur acl push` does once a person confirms its preview (§8.8): the plan over its verified state and the config head,
// then the save with the plan's digest as a confirmed act, its base published through api; synced at the base's pin.
// A base past §7.9's config_bases_per_min is refused 429 by api, which a caller waits out and saves again.
func (self *T3Network) SaveAclPolicy(ctx context.Context, device *T3Device, source []byte) (*bridge.AuthPin, error) {
	plan, err := device.Sidecar().PlanAclPolicy(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("%s's ACL plan: %w", device.Name, err)
	}
	if err := plan.Tests.Err(); err != nil {
		return nil, fmt.Errorf("%s's ACL document fails its tests: %w", device.Name, err)
	}
	pin, err := device.Sidecar().SaveAclPolicy(ctx, source, plan.Digest, sidecar.SidecarActConfirmed)
	if err != nil {
		return nil, fmt.Errorf("%s's ACL save: %w", device.Name, err)
	}
	return pin, self.Sync(ctx, pin.GetVersion())
}

// admin grants subject, a sidecar or a member, role on repoId by a RepoGrant it files (§8.8), synced at its entry.
func (self *T3Network) Grant(ctx context.Context, admin *T3Device, repoId sidecar.Id, subject sidecar.Id, role protocol.RepoGrantRole) (*bridge.AuthPin, error) {
	return self.FileStatement(ctx, admin, bridge.SignedType_SIGNED_REPO_GRANT, &protocol.RepoGrant{
		RepoId:  repoId.Bytes(),
		Subject: subject.Bytes(),
		Role:    role,
	})
}

// One sidecar of a world's network: the T3Sidecar, its key client and, once it follows, its follower and EvsClient.
type T3Device struct {
	*T3Sidecar
	Network *T3Network
	// The member whose credential enrolled it (T3EnrollArgs), zero for a tagged device.
	MemberId sidecar.Id
	// The tag ids its auth key carried, 32 lowercase hex each; none for a member's device.
	TagIds []string
	// Its key client: FakeGkm's client for its identity, signing by its identity key (the file's keys row), or with
	// RealKeys its real one (keys).
	Gkm sidecar.GkmClient
	// With RealKeys, its key and evs clients as ur daemon composes them (world_keys.go); nil otherwise.
	keys *t3WorldKeys

	stateLock sync.Mutex
	follower  *sidecar.SidecarConfigFollower
	evs       *sidecar.EvsClient
}

// The token api minted the device, as a bearer: what it presents to api itself.
func (self *T3Device) Bearer() string {
	return "Bearer " + self.Network.world.api.ClientToken(self.Sidecar().Identity().ClientId)
}

// Its config follower, nil before it follows.
func (self *T3Device) Follower() *sidecar.SidecarConfigFollower {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.follower
}

// The network's config heads as a device's follower reads them, with a ref the device saves a confirmed ACL document
// through (sidecar.ConfirmedAclConfigRefProvider): the heads' ref signing as the device, which only a device the log
// names a config writer may use (CheckConfigHeadWriter), and which runs no periodic fold, as a native host's
// read-only follower with a ref dedicated to explicit ACL confirmation.
type t3WorldConfigHeads struct {
	*sidecartest.FakeConfigHeads
	ref *sidecartest.FakeConfigHeadRef
}

func (self *t3WorldConfigHeads) ConfirmedAclConfigRef() sidecar.ConfigHeadRef {
	return self.ref
}

// Trusts genesis and follows the log on the network's config heads with the device's key client, which the follower
// hands each verified state, until it stands.
func (self *T3Device) follow(ctx context.Context, genesis [sha256.Size]byte) error {
	running := self.Sidecar()
	if err := running.TrustFirstUse(genesis[:]); err != nil {
		return fmt.Errorf("%s's anchor: %w", self.Name, err)
	}
	var heads sidecar.ConfigHeadReader = &t3WorldConfigHeads{
		FakeConfigHeads: self.Network.heads,
		ref: self.Network.heads.ForSigner(running.Identity().SidecarId, func(purpose string, signedType bridge.SignedType, body []byte) (*bridge.Signed, error) {
			return running.Sign(purpose, signedType, body, nil)
		}),
	}
	var followerSettings *sidecar.SidecarConfigFollowerSettings
	switch {
	case self.keys != nil:
		// the config repo's heads on evs, read through the device's own EvsClient, as ur daemon's follower reads them
		heads = self.keys.heads
		followerSettings = t3WorldKeysFollowerSettings()
	case self.Network.world.settings.FollowerSpendCheck:
		followerSettings = t3WorldSpendFollowerSettings()
	}
	follower, err := running.FollowConfig(&sidecar.SidecarConfigFollowerArgs{Heads: heads, Gkm: self.Gkm, Settings: followerSettings})
	if err != nil {
		return fmt.Errorf("%s's follower: %w", self.Name, err)
	}
	timeout := self.Network.world.settings.SyncTimeout
	if self.keys != nil && 0 < self.Network.world.settings.KeyJoinTimeout {
		timeout = self.Network.world.settings.KeyJoinTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := follower.WaitStanding(ctx); err != nil {
		status := follower.Status()
		follower.Close()
		return fmt.Errorf("%s's follower did not stand, %s (%s): %w", self.Name, status.State, status.Reason, err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.follower = follower
	return nil
}

// The device's EvsClient, made at the first call as a host makes it (Sidecar.EvsClientArgs): SidecarApi's transport,
// so every request reaches the evs the world serves at its origin, the device's service tokens from server's
// exchange, its identity key, its key client, its follower's verified state, and its statements filed as
// FileStatement files them. Its follower must stand.
func (self *T3Device) EvsClient() (*sidecar.EvsClient, error) {
	evs, follower := func() (*sidecar.EvsClient, *sidecar.SidecarConfigFollower) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.evs, self.follower
	}()
	if evs != nil {
		return evs, nil
	}
	if follower == nil {
		return nil, fmt.Errorf("%s reads evs by a follower that stands", self.Name)
	}
	if self.keys != nil {
		return self.keys.evsClient()
	}
	config := func() sidecar.GkmAuthView {
		// never a typed nil: no state verifies before the first
		if state := follower.VerifiedState(); state != nil {
			return state
		}
		return nil
	}
	fileStatement := func(ctx context.Context, signedType bridge.SignedType, body proto.Message) error {
		_, err := self.Network.FileStatement(ctx, self, signedType, body)
		return err
	}
	args, err := self.Sidecar().EvsClientArgs(self.Gkm, config, fileStatement)
	if err != nil {
		return nil, err
	}
	client, err := sidecar.NewEvsClient(args)
	if err != nil {
		return nil, err
	}
	// a racing caller's, if it made one first
	held := func() *sidecar.EvsClient {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.evs == nil {
			self.evs = client
		}
		return self.evs
	}()
	if held != client {
		client.Close()
	}
	return held, nil
}

// Ends the device's EvsClient and follower, then the sidecar.
func (self *T3Device) close() {
	var evs *sidecar.EvsClient
	var follower *sidecar.SidecarConfigFollower
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		evs, follower = self.evs, self.follower
		self.evs, self.follower = nil, nil
	}()
	if evs != nil {
		evs.Close()
	}
	if follower != nil {
		follower.Close()
	}
	if self.keys != nil {
		self.keys.close()
	}
	self.T3Sidecar.Close()
}

// GET and POST /network/config/projection with one device's token, as SidecarApi sends them
// (sidecar.ConfigProjectionReader and ConfigBasePublisher), decoding what server's controller answers.
type t3NetworkRoutes struct {
	api    *T3Api
	bearer string
}

func (self *t3NetworkRoutes) ReadConfigProjection(ctx context.Context, after uint64) (*sidecar.ConfigProjection, error) {
	path := "/network/config/projection?" + url.Values{"after": []string{strconv.FormatUint(after, 10)}}.Encode()
	status, answer, err := self.api.Call(ctx, http.MethodGet, path, self.bearer, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET /network/config/projection: %d", status)
	}
	result := &controller.NetworkConfigProjectionResult{}
	if err := json.Unmarshal(answer, result); err != nil {
		return nil, err
	}
	projection := &sidecar.ConfigProjection{SignedGenesis: result.Genesis, Entries: result.Entries, Rows: result.Rows, More: result.More}
	if result.Tip != nil {
		projection.Tip = &bridge.AuthPin{Version: result.Tip.N, Chain: result.Tip.Chain}
	}
	return projection, nil
}

func (self *t3NetworkRoutes) PublishConfigBase(ctx context.Context, base *sidecar.ConfigBasePublish) (*bridge.AuthPin, error) {
	status, answer, err := self.api.Call(ctx, http.MethodPost, "/network/config/projection", self.bearer, &controller.NetworkConfigProjectionPublishArgs{
		SignedHeader: base.SignedHeader,
		Rows:         base.Rows,
		Genesis:      base.Genesis,
		ConfigPushes: base.ConfigPushes,
	})
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: %s", sidecar.ErrConfigTipMoved, answer)
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", sidecar.ErrConfigRefused, answer)
	default:
		return nil, fmt.Errorf("POST /network/config/projection: %d %s", status, answer)
	}
	result := &controller.NetworkConfigPinResult{}
	if err := json.Unmarshal(answer, result); err != nil {
		return nil, err
	}
	return &bridge.AuthPin{Version: result.N, Chain: result.Chain}, nil
}
