//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's smoke test (e2e-world): every block of the world (world_test.go) started on its stores, a few real
// sidecars in two networks, and one step through each service, each asserting its outcome:
//
//   - enroll: every sidecar of both networks enrolled through api from its owner's auth key, Online on the exchange,
//     admitted by its network's owner and following the network's log at the admissions;
//   - evs: in each network a shared repo the owner created through evs, every sidecar writes its own path and reads
//     every other sidecar's back at the version its writer was answered, and the second network reads none of the
//     first's, refused by its own client and by evs's 403 to its token;
//   - gkm, on a world of real keys (T3WorldSettings.RealKeys, TestE2eWorldSmokeRealKeys until it is the world's
//     default): each network's repo, whose creation formed its group at gkm's block, held at generation 1 by every
//     sidecar's own key client, rotated by its owner, and held at generation 2 under the rotation's group and
//     commitment by every sidecar, which writes under it and reads every other's write;
//   - mcp: a tools/list call with an access token the world's issuer minted for mcp's origin lists mcp's tools, and
//     the same call with no token is answered 401 with the protected resource's challenge;
//   - dns: the first network's owner saves an ACL document granting its members admin on dns:smoke, as `ur acl push`
//     does once confirmed (its base waiting out §7.9's one base a minute), writes smoke's TXT record in its zone through
//     dns's api, since §21.7's probation publishes no address record for a new network, and the dns edge answers it
//     signed over UDP; the second network's owner is refused the same name, and the edge still answers the first's;
//   - ledgers: the metadata cluster's write budget holds the evs block's spend for the writes, and api's own budget
//     its sequencer's spend for the networks' statements;
//   - chaos: evs stopped mid-run and started again on its stores, after which every write reads back as it was
//     answered, through the restarted block;
//   - the quota ledgers: evs's drain at the restart sent its usage window through api's quota surface, so each
//     network's quota state holds the bytes its writes stored, and its transfer was drawn from its balance, never past
//     it, every byte drawn a debit on a meter (the evs meter's here), exactly;
//   - sockets and tlsedge: the second network, made Pro by a paid Pro balance, saves a standing public publish of
//     tls/443/web, and its member device publishes the name as a raw socket listener; a visitor through tlsedge's 443,
//     behind the lb's PROXY header, completes TLS with the device's own key and is answered by the device, and a host
//     no grant publishes is refused at the edge;
//   - mail (world_smoke_mail_test.go): the first network turns mail on, its receiver serves smtp/25/mx once its TLSA
//     is live, and an internet sender's message reaches the member's mailbox through the world's lb, mxedge's router
//     and the receiver, held to the dns edge's signed TLSA, and is read back by the member's reader; then the second
//     network, aged past probation at its enrollment, turns mail on and its owner's device sends to the first's alias
//     through smtpedge, the LAN splice to mxedge's router and the receiver, and the first network's reader reads it.
//
// Each phase has its bound and dumps the world's progress when it stalls; the case has the conventions' 10 minutes.
// Run it through the build's wrapper: -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/sidecart3"
)

// What the smoke test runs with (QD-702).
type worldSmokeSettings struct {
	// The networks and the sidecars of each.
	NetworkCount      int
	ClientsPerNetwork int
	// Each phase's bound.
	PhaseTimeout time.Duration
	// The bursts' pauses and seed.
	Bursts *WorldBurstSettings
	// The JSON-RPC body of the mcp call, and the tools its answer must list.
	McpToolsListBody string
	McpToolNames     []string
	// How long the mcp call's access token lives: the issuer's own default is minted, this bounds nothing but the
	// call's wait.
	McpCallTimeout time.Duration
	// The grant of the first network's ACL document that gives its members admin on the record's host, beside the mail
	// step's grants, the host and the text its TXT record holds, and the text the second network's refused write
	// names: one character-string each, so no space, which the API's presentation format reads as two.
	DnsAclGrant    string
	DnsRecordHost  string
	DnsRecordText  string
	DnsForeignText string
	// The second network's ACL document, given the time its tls/ grant expires: its members publish and listen on the
	// tls/ host under the public audience in raw mode, a standing publish, which §8.5 holds to a Pro or Team document,
	// beside the mail step's grants, since the network sends mail too.
	// The tls/ host, the one no grant names that a visitor is refused, the grant's life, the visitor's request line,
	// and the TLS helpers' bounds.
	TlsAclPolicy func(expires time.Time) string
	// The TXT record the second network writes in its zone where its label's map serial is still 0 (the step's
	// finding), whose write stamps the label into the map's serial order, and its text.
	TlsRecordHost      string
	TlsRecordText      string
	TlsHost            string
	TlsUnpublishedHost string
	TlsGrantLife       time.Duration
	TlsRequest         string
	Tls                *WorldTlsSettings
	// The gkm step's waits for each sidecar's key client.
	Keys *worldKeysSettings
	// The mail step's grants, addresses, message and bounds (world_smoke_mail_test.go).
	Mail *worldSmokeMailSettings
}

func defaultWorldSmokeSettings() *worldSmokeSettings {
	return &worldSmokeSettings{
		NetworkCount:      2,
		ClientsPerNetwork: 3,
		PhaseTimeout:      4 * time.Minute,
		Bursts:            DefaultWorldBurstSettings(),
		McpToolsListBody:  `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		McpToolNames:      []string{"providerLocations", "fetch"},
		McpCallTimeout:    60 * time.Second,
		DnsAclGrant:       `{"src": ["autogroup:member"], "dst": ["dns:smoke"], "allow": ["admin"]}`,
		DnsRecordHost:     "smoke",
		DnsRecordText:     "e2e-world-smoke",
		DnsForeignText:    "written-from-another-network",
		TlsAclPolicy: func(expires time.Time) string {
			return worldAclPolicy(append([]string{
				fmt.Sprintf(`{"src": ["autogroup:member"], "dst": ["dns:web", "sock:tls/443/web"], "allow": ["publish", "listen"], "audience": "public", "app_mode": "raw", "expires": %q}`, expires.UTC().Format(time.RFC3339)),
				`{"src": ["autogroup:member"], "dst": ["dns:about"], "allow": ["admin"]}`,
			}, defaultWorldSmokeMailSettings().AclGrants...)...)
		},
		TlsRecordHost:      "about",
		TlsRecordText:      "e2e-world-tls",
		TlsHost:            "web",
		TlsUnpublishedHost: "shop",
		TlsGrantLife:       12 * time.Hour,
		TlsRequest:         "hello through tlsedge",
		Tls:                DefaultWorldTlsSettings(),
		Keys:               defaultWorldKeysSettings(),
		Mail:               defaultWorldSmokeMailSettings(),
	}
}

// One sidecar's write in the smoke's evs step: its path, its content and what its writer was answered.
type worldSmokeWrite struct {
	device  *sidecart3.T3Device
	path    string
	content []byte
	result  *sidecar.EvsWriteResult
}

// Every block, two networks of three sidecars, and one step through each service the world wires end to end, on the
// world's defaults. See the file's header.
func TestE2eWorldSmoke(t *testing.T) {
	runWorldSmoke(t, DefaultWorldSettings())
}

// The smoke on a world whose sidecar.yml turns §8.10's contract gate on (WorldSettings.AclContractGate), so every
// contract connect makes for the world's streams, the edges' to a published listener among them, is judged by the
// gate over api's gate cache, which the world installs.
func TestE2eWorldSmokeUnderTheContractGate(t *testing.T) {
	worldSettings := DefaultWorldSettings()
	worldSettings.AclContractGate = true
	runWorldSmoke(t, worldSettings)
}

// The smoke on a world of worldSettings, its networks and clients the smoke's own. The gkm step runs on a world of
// real keys alone (T3WorldSettings.RealKeys).
func runWorldSmoke(t *testing.T, worldSettings *WorldSettings) {
	settings := defaultWorldSmokeSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		worldSettings.NetworkCount = settings.NetworkCount
		worldSettings.ClientsPerNetwork = settings.ClientsPerNetwork
		world := NewWorld(t, worldSettings)
		defer world.Close()
		if active := worldContractGateActive(); active != worldSettings.AclContractGate {
			t.Fatalf("the contract gate is active %t on a world whose sidecar.yml turns it %t", active, worldSettings.AclContractGate)
		}

		// enroll
		ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
		networks, err := world.AddNetworks(ctx, nil)
		cancel()
		if err != nil {
			world.FailProgress(t, "the enrollment", err, nil)
		}
		for _, network := range networks {
			if len(network.Clients) != settings.ClientsPerNetwork {
				t.Fatalf("%s enrolled %d sidecars, not %d", network.Name, len(network.Clients), settings.ClientsPerNetwork)
			}
			for _, device := range network.Clients {
				status := device.Sidecar().Status()
				identity := device.Sidecar().Identity()
				if status.State != sidecar.SidecarStateOnline || identity == nil || server.Id(identity.NetworkId) != network.NetworkId {
					t.Fatalf("%s is %s in %v after its enrollment", device.Name, status.State, identity)
				}
				if device.Follower() == nil || device.Follower().Client() == nil {
					t.Fatalf("%s follows no log after its admission", device.Name)
				}
			}
		}

		// the second network sends internet mail in the mail step, which §21.7's probation refuses a network under 7
		// days old, so it is aged before anything reads its standing and caches it
		if err := worldAgeNetwork(t.Context(), networks[1].NetworkId, settings.Mail.SenderNetworkAge); err != nil {
			t.Fatal(err)
		}

		// evs: a shared repo in each network, every sidecar's write read back by every other
		writesByNetwork := map[*WorldNetwork][]*worldSmokeWrite{}
		repoIds := map[*WorldNetwork]sidecar.Id{}
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		for _, network := range networks {
			repoId, err := network.CreateSharedRepo(ctx, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER)
			if err != nil {
				cancel()
				world.FailProgress(t, network.Name+"'s shared repo", err, nil)
			}
			repoIds[network] = repoId
			for _, device := range network.Clients {
				writesByNetwork[network] = append(writesByNetwork[network], &worldSmokeWrite{
					device:  device,
					path:    device.Name + ".txt",
					content: []byte(fmt.Sprintf("%s's write in %s\n", device.Name, network.Name)),
				})
			}
		}
		bursts := []*WorldBurst{}
		for _, network := range networks {
			for _, write := range writesByNetwork[network] {
				bursts = append(bursts, &WorldBurst{
					Name:      write.path,
					StepCount: 1,
					Step: func(ctx context.Context, index int, random *rand.Rand) error {
						client, err := write.device.EvsClient()
						if err != nil {
							return err
						}
						result, err := client.Repo(repoIds[network]).Write(ctx, &sidecar.EvsWriteArgs{
							Path:    write.path,
							Content: bytes.NewReader(write.content),
							Size:    int64(len(write.content)),
						})
						if err != nil {
							return err
						}
						write.result = result
						return nil
					},
				})
			}
		}
		if err := RunBursts(ctx, t, "the evs writes", bursts, settings.Bursts); err != nil {
			cancel()
			world.FailProgress(t, "the evs writes", err, bursts)
		}
		FailBurstErrors(t, bursts)
		if t.Failed() {
			cancel()
			t.FailNow()
		}
		readAll := func(phase string) {
			for _, network := range networks {
				for _, reader := range network.Clients {
					client, err := reader.EvsClient()
					if err != nil {
						t.Fatalf("%s's evs client: %v", reader.Name, err)
					}
					for _, write := range writesByNetwork[network] {
						var content bytes.Buffer
						info, err := client.Repo(repoIds[network]).ReadHead(ctx, &sidecar.EvsReadArgs{Path: write.path, Fresh: true}, &content)
						if err != nil {
							world.FailProgress(t, phase, fmt.Errorf("%s's read of %s: %w", reader.Name, write.path, err), nil)
						}
						if !bytes.Equal(content.Bytes(), write.content) || info == nil || info.Version != write.result.PathVersion {
							t.Fatalf("%s: %s reads %s as %q at version %v, not %q at %d", phase, reader.Name, write.path, content.String(), info, write.content, write.result.PathVersion)
						}
					}
				}
			}
		}
		writesEnded := time.Now()
		readAll("the evs reads")
		// the budget's ledger holds the writes' spend once each process that spent it has reported since
		ledgers, err := world.ReadLedgers(ctx, writesEnded)
		if err != nil {
			t.Fatalf("the ledgers: %v", err)
		}
		spent := int64(0)
		for clusterId := range ledgers.BudgetReports {
			spent += ledgers.BudgetByteCount(clusterId)
		}
		if spent <= 0 {
			reports, _ := json.Marshal(ledgers.BudgetReports)
			t.Fatalf("no block reported the writes against the metadata clusters' budget: %s", reports)
		}
		// api's sequencer spent its statements' objects on its own budget, P0 every one, beside the blocks'
		apiSpent := int64(0)
		for clusterId := range ledgers.BudgetReports {
			apiSpent += ledgers.ApiBudgetByteCount(clusterId)
		}
		if ledgers.ApiProcessId == "" || apiSpent <= 0 {
			reports, _ := json.Marshal(ledgers.BudgetReports)
			t.Fatalf("api's process %q reported no spend for the networks' statements: %s", ledgers.ApiProcessId, reports)
		}
		// the second network reads none of the first's, at its client and at evs
		first, second := networks[0], networks[1]
		for _, device := range second.Clients {
			client, err := device.EvsClient()
			if err != nil {
				t.Fatal(err)
			}
			var content bytes.Buffer
			if _, err := client.Repo(repoIds[first]).ReadHead(ctx, &sidecar.EvsReadArgs{Path: writesByNetwork[first][0].path}, &content); err == nil || 0 < content.Len() {
				t.Fatalf("%s of %s read %s's repo", device.Name, second.Name, first.Name)
			}
			token, err := device.Sidecar().ServiceToken(ctx, world.T3().Api().ServiceOrigin("evs"), []string{"repo:" + repoIds[second].String() + ":r"})
			if err != nil {
				t.Fatalf("%s's read token for its own repo: %v", device.Name, err)
			}
			if status, _ := worldCall(ctx, world, http.MethodGet, world.T3().Api().ServiceOrigin("evs")+"/evs/v1/repos/"+sidecar.EvsKeyId(repoIds[first]), token, ""); status != http.StatusForbidden {
				t.Fatalf("evs answered %s of %s %d at %s's repo, not 403", device.Name, second.Name, status, first.Name)
			}
		}
		cancel()

		// gkm: on real keys, each network's repo's group formed at gkm's block, generation 1 held by every sidecar, then
		// rotated by its owner, generation 2 held by every sidecar, and written and read under
		if world.T3().Settings().RealKeys {
			worldSmokeGkm(t, world, settings, networks, repoIds)
		} else {
			t.Logf("the gkm step runs on a world of real keys alone (TestE2eWorldSmokeRealKeys): T3WorldSettings.RealKeys is off by default")
		}

		// mcp: tools/list with the issuer's access token for mcp's origin, and 401 with none
		ctx, cancel = world.StepContext(t.Context(), settings.McpCallTimeout)
		mcpOrigin := world.T3().Api().ServiceOrigin("mcp")
		accessToken, _, err := oauth.MintAccessToken(&oauth.MintAccessTokenArgs{
			UserId:    first.OwnerUserId,
			NetworkId: first.NetworkId,
			ClientId:  "e2e-world",
			Audience:  mcpOrigin,
			Scopes:    []string{oauth.ScopeMcpRead},
		})
		if err != nil {
			cancel()
			t.Fatalf("the mcp access token: %v", err)
		}
		status, answer := worldCall(ctx, world, http.MethodPost, mcpOrigin+"/", accessToken, settings.McpToolsListBody)
		if status != http.StatusOK {
			cancel()
			t.Fatalf("mcp answered tools/list %d: %s", status, answer)
		}
		listed := &struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}{}
		if err := json.Unmarshal(answer, listed); err != nil {
			cancel()
			t.Fatalf("mcp's tools/list answer: %v: %s", err, answer)
		}
		toolNames := []string{}
		for _, tool := range listed.Result.Tools {
			toolNames = append(toolNames, tool.Name)
		}
		for _, name := range settings.McpToolNames {
			if !slices.Contains(toolNames, name) {
				t.Fatalf("mcp lists %v, without %s", toolNames, name)
			}
		}
		if status, answer := worldCall(ctx, world, http.MethodPost, mcpOrigin+"/", "", settings.McpToolsListBody); status != http.StatusUnauthorized {
			t.Fatalf("mcp answered a tools/list with no token %d, not 401: %s", status, answer)
		}
		cancel()

		// dns: the first network's owner saves an ACL document granting its members admin on dns:smoke, writes smoke's A
		// record through dns's api, and the dns edge answers it signed; the second network's owner writes nothing there
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		// the mail step's grants in the same document, so the network saves one base for both (§7.9's one a minute)
		if _, err := world.SaveAclPolicy(ctx, first, first.Owner(), worldAclPolicy(append([]string{settings.DnsAclGrant}, settings.Mail.AclGrants...)...)); err != nil {
			cancel()
			world.FailProgress(t, "the dns grant", err, nil)
		}
		dnsClient, err := first.Owner().Sidecar().Dns()
		if err != nil {
			cancel()
			t.Fatalf("%s's dns client: %v", first.Owner().Name, err)
		}
		zone, err := dnsClient.Zone(ctx)
		if err != nil || zone.Apex() == "" || dns.CanonicalName(zone.Root) != dns.CanonicalName(world.HostsSettings().DnsRoot) {
			cancel()
			t.Fatalf("%s's zone: %+v, %v", first.Name, zone, err)
		}
		recordName := settings.DnsRecordHost + "." + zone.Apex()
		// a TXT record: a new network is on §21.7's probation, which publishes no A, AAAA or CNAME
		if _, err := world.SetDnsRecord(ctx, first.Owner(), &sidecar.DnsRecordUpsert{Name: recordName, Type: "TXT", Values: []string{settings.DnsRecordText}}, nil); err != nil {
			cancel()
			world.FailProgress(t, "the dns write", err, nil)
		}
		if answer, err := world.AwaitDnsAnswer(ctx, recordName, dns.TypeTXT, func(answer *dns.Msg) bool {
			return dnsAnswerHoldsSignedTxt(answer, recordName, settings.DnsRecordText)
		}, nil); err != nil {
			cancel()
			t.Fatalf("the dns edge's answer for %s: %v: %v", recordName, err, answer)
		}
		if _, err := world.SetDnsRecord(ctx, second.Owner(), &sidecar.DnsRecordUpsert{Name: recordName, Type: "TXT", Values: []string{settings.DnsForeignText}}, nil); err == nil {
			cancel()
			t.Fatalf("%s of %s wrote %s in %s's zone", second.Owner().Name, second.Name, recordName, first.Name)
		}
		if answer, err := world.DnsQuery(ctx, recordName, dns.TypeTXT, nil); err != nil || !dnsAnswerHoldsSignedTxt(answer, recordName, settings.DnsRecordText) {
			cancel()
			t.Fatalf("the dns edge answers %s with %v after a refused write: %v", recordName, answer, err)
		}
		cancel()

		// chaos: evs stopped mid-run and started again on its stores, every write read back through it
		if err := world.Restart(WorldBlockEvs); err != nil {
			world.FailProgress(t, "evs's restart", err, nil)
		}
		if runs := world.Block(WorldBlockEvs).RunCount(); runs != 2 {
			t.Fatalf("evs ran %d times after one restart", runs)
		}
		ctx, cancel = world.StepContext(t.Context(), settings.PhaseTimeout)
		readAll("the evs reads after evs's restart")

		// ledgers: evs's drain sent its usage window, so each network holds what its writes stored, its transfer drawn
		// from its balance and never past it
		ledgers, err = world.ReadLedgers(ctx, time.Time{})
		cancel()
		if err != nil {
			t.Fatalf("the ledgers: %v", err)
		}
		for _, network := range networks {
			ledger := ledgers.Networks[network.NetworkId]
			switch {
			case ledger == nil:
				t.Fatalf("%s has no quota ledger", network.Name)
			case ledger.StoredByteCount <= 0:
				t.Fatalf("%s's quota ledger stores nothing after its writes: %+v", network.Name, ledger)
			case ledger.DebitedByteCount() <= 0 || ledger.BalanceByteCount <= 0 || ledger.Spent:
				t.Fatalf("%s's balance after its writes and reads: %+v", network.Name, ledger)
			}
			// every byte drawn from its balances is a service debit on a meter, the evs meter's for its writes and reads
			if ledgers.Meters.ByteCount(network.NetworkId, sidecar.QuotaMeterEvs) <= 0 || ledgers.Meters.TotalByteCount(network.NetworkId) != ledger.DebitedByteCount() {
				t.Fatalf("%s's balances were debited %d, its meters: %s", network.Name, ledger.DebitedByteCount(), ledgers.Meters.Describe(network.NetworkId))
			}
		}

		// sockets and tlsedge: the second network, made Pro, publishes a member's raw tls/ listener on its standing
		// grant, which a visitor reaches through tlsedge's 443
		worldSmokeTls(t, world, settings, second)

		// mail: the first network turns mail on and receives an internet sender's message through mxedge's router, then
		// the second, verified by its Pro balance, sends to it through smtpedge
		mailbox := worldSmokeMailInbound(t, world, settings, first, append([]string{settings.DnsAclGrant}, settings.Mail.AclGrants...))
		worldSmokeMailOutbound(t, world, settings, second, mailbox)
	})
}

// The smoke's gkm step, on real keys: each network's shared repo, whose KF1 its creation ran through the owner's key
// client at gkm's block, held at generation 1 under one group by every sidecar's own key client; the owner rotates it
// (`ur keys rotate`), and every sidecar holds generation 2 under the rotation's group and commitment, writes under it
// and reads every other sidecar's write.
func worldSmokeGkm(t testing.TB, world *World, settings *worldSmokeSettings, networks []*WorldNetwork, repoIds map[*WorldNetwork]sidecar.Id) {
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	for _, network := range networks {
		repoId := repoIds[network]
		groupIds := map[sidecar.Id]bool{}
		for _, device := range network.Clients {
			key, err := worldKeysAwaitGeneration(ctx, device, repoId, 1, settings.Keys)
			if err != nil {
				world.FailProgress(t, network.Name+"'s generation 1", err, nil)
			}
			groupIds[key.GroupId()] = true
		}
		if len(groupIds) != 1 {
			t.Fatalf("%s's sidecars hold its repo under %d groups", network.Name, len(groupIds))
		}
		rotated, err := network.Owner().Gkm.RotateRepo(ctx, repoId)
		if err != nil {
			world.FailProgress(t, network.Name+"'s rotation", err, nil)
		}
		if rotated.Generation() != 2 {
			t.Fatalf("%s's rotation made generation %d, not 2", network.Name, rotated.Generation())
		}
		for _, device := range network.Clients {
			key, err := worldKeysAwaitGeneration(ctx, device, repoId, 2, settings.Keys)
			if err != nil {
				world.FailProgress(t, network.Name+"'s generation 2", err, nil)
			}
			if key.GroupId() != rotated.GroupId() || !bytes.Equal(key.Commitment(), rotated.Commitment()) {
				t.Fatalf("%s holds generation 2 under group %s, commitment %x; the rotation made %s, %x", device.Name, key.GroupId(), key.Commitment(), rotated.GroupId(), rotated.Commitment())
			}
		}
		content := func(device *sidecart3.T3Device) []byte {
			return []byte(fmt.Sprintf("%s's write under generation 2\n", device.Name))
		}
		for _, device := range network.Clients {
			if err := worldKeysWrite(ctx, device, repoId, device.Name+"-generation-2.txt", content(device)); err != nil {
				world.FailProgress(t, device.Name+"'s write under generation 2", err, nil)
			}
		}
		for _, reader := range network.Clients {
			for _, writer := range network.Clients {
				if err := worldKeysRead(ctx, reader, repoId, writer.Name+"-generation-2.txt", content(writer)); err != nil {
					world.FailProgress(t, "the reads under generation 2", err, nil)
				}
			}
		}
	}
}

// The smoke's sockets and tlsedge step: network, made Pro by a paid Pro balance, so verified and its document able to
// hold a standing publish (§8.5), saves its members' public publish of tls/443/<TlsHost> in raw mode; its member
// device's certificate row is issued for the network's pair, and the device starts its socket component and §16.6's
// approval gate and listens on the name; a visitor through tlsedge's direct 443, behind the lb's PROXY header,
// completes TLS with the device's own key and is answered by the device; a visitor for a host no grant publishes is
// refused at the edge.
func worldSmokeTls(t testing.TB, world *World, settings *worldSmokeSettings, network *WorldNetwork) {
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	device := network.Clients[len(network.Clients)-1]
	if err := world.VerifyNetwork(ctx, network, true, settings.Tls); err != nil {
		t.Fatalf("%s's Pro balance: %v", network.Name, err)
	}
	if _, err := world.SaveAclPolicy(ctx, network, network.Owner(), settings.TlsAclPolicy(time.Now().Add(settings.TlsGrantLife))); err != nil {
		world.FailProgress(t, "the tls/ grant", err, nil)
	}
	dnsClient, err := network.Owner().Sidecar().Dns()
	if err != nil {
		t.Fatalf("%s's dns client: %v", network.Owner().Name, err)
	}
	zoneAnswer, err := dnsClient.Zone(ctx)
	if err != nil || zoneAnswer.Apex() == "" {
		t.Fatalf("%s's zone: %+v, %v", network.Name, zoneAnswer, err)
	}
	zone := zoneAnswer.Apex()
	if err := world.IssueTlsCertificateRow(ctx, device, zoneAnswer, settings.Tls); err != nil {
		t.Fatalf("%s's certificate row: %v", device.Name, err)
	}
	if err := world.StartSockets(ctx, device); err != nil {
		t.Fatal(err)
	}
	if _, err := world.InstallApprovalGate(ctx, device); err != nil {
		t.Fatal(err)
	}
	name := "tls/443/" + settings.TlsHost
	listenSettings := &sidecar.SocketListenSettings{AppMode: "raw"}
	listener, err := world.Listen(ctx, device, name, listenSettings, settings.Tls)
	if errors.Is(err, sidecar.ErrSidecarActionInvalid) {
		// a product finding, which the step reports and carries on past to the servers through a stand-in gate
		t.Errorf("§16.6's approval gate refused %s's Sidecar.Listen on %s: %v; socketListenAction builds the listen's action with no Effect, which ApprovalGate.Authorize refuses before any grant or decision is read", device.Name, name, err)
		device.Sidecar().InstallGate(worldListenGate{})
		listener, err = world.Listen(ctx, device, name, listenSettings, settings.Tls)
	}
	if err != nil {
		world.FailProgress(t, "the tls/ listen", err, nil)
	}
	serverName := worldTlsServerName(settings.TlsHost, zone)
	answer := func(line []byte) []byte {
		return fmt.Appendf(nil, "%s: %s\n", device.Name, line)
	}
	echo, err := worldServeTlsEcho(device, listener, serverName, answer, settings.Tls)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	defer func() {
		listener.Close()
		echo.Wait()
	}()
	spkiSha256, err := worldTlsKeySpkiSha256(device)
	if err != nil {
		t.Fatal(err)
	}
	// tlsedge routes a server name by the label map it follows since its last serial (DnsLabelMapSince), so a label
	// whose change serial is still 0 never reaches a running tlsedge
	serial, found, err := worldDnsLabelChangeSerial(ctx, network.NetworkId, zoneAnswer.Root)
	if err != nil || !found {
		t.Fatalf("%s's label in the map: found %t, %v", network.Name, found, err)
	}
	if serial == 0 {
		// a product finding, which the step reports and carries on past: a record written in the zone stamps the label
		t.Errorf("%s's label %s holds change serial 0 in %s's label map, so DnsLabelMapSince never returns it and tlsedge, which follows the map from its start, never routes %s: the label was drawn outside the zone's serial order (model.EnsureNetworkLabelInTx stamps no serial, and ensureNetworkZoneInTx stamps one only when the zone's system rows change)", network.Name, zoneAnswer.Label, zoneAnswer.Root, worldTlsServerName(settings.TlsHost, zone))
		recordName := settings.TlsRecordHost + "." + zone
		if _, err := world.SetDnsRecord(ctx, network.Owner(), &sidecar.DnsRecordUpsert{Name: recordName, Type: "TXT", Values: []string{settings.TlsRecordText}}, nil); err != nil {
			world.FailProgress(t, "the record that stamps the label", err, nil)
		}
	}
	request := []byte(settings.TlsRequest)
	if err := world.AwaitTlsVisit(ctx, serverName, spkiSha256, request, bytes.TrimSuffix(answer(request), []byte("\n")), settings.Tls); err != nil {
		world.FailProgress(t, "the visit through tlsedge", fmt.Errorf("%w; the listener's accepts ended: %v; %s", err, echo.Ended(), worldTlsServerState(t.Context(), network.NetworkId, zoneAnswer.Root)), nil)
	}
	// a host no grant publishes: tlsedge finds no listener, and the visitor is refused in the handshake
	unpublished := worldTlsServerName(settings.TlsUnpublishedHost, zone)
	if answered, err := world.VisitTls(ctx, unpublished, spkiSha256, request, settings.Tls); err == nil {
		t.Fatalf("a visitor to %s, which no grant publishes, was answered %q", unpublished, answered)
	}
}

// One request through T3Api to url with token as its bearer, "" for none, and body as JSON, "" for none: its status
// and answer.
func worldCall(ctx context.Context, world *World, method string, url string, token string, body string) (int, []byte) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, []byte(err.Error())
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
	}
	response, err := world.T3().Api().Transport().RoundTrip(request)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil && !errors.Is(err, io.EOF) {
		return response.StatusCode, []byte(err.Error())
	}
	return response.StatusCode, answer
}

// Decodes reader's JSON into value.
func decodeJson(reader io.Reader, value any) error {
	return json.NewDecoder(reader).Decode(value)
}
