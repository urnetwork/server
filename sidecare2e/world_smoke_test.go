//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's smoke test (e2e-world): every block of the world (world_test.go) started on its stores, a few real
// sidecars in two networks, and one step through each service, each asserting its outcome:
//
//   - enroll: every sidecar of both networks enrolled through api from its owner's auth key, Online on the exchange,
//     admitted by its network's owner and following the network's log at the admissions;
//   - evs: in each network a shared repo the owner created through evs, every sidecar writes its own path and reads
//     every other sidecar's back at the version its writer was answered, and the second network reads none of the
//     first's, refused by its own client and by evs's 403 to its token;
//   - mcp: a tools/list call with an access token the world's issuer minted for mcp's origin lists mcp's tools, and
//     the same call with no token is answered 401 with the protected resource's challenge;
//   - dns: the first network's owner saves an ACL document granting its members admin on dns:smoke, as `ur acl push`
//     does once confirmed (its base waiting out §7.9's one base a minute), writes smoke's TXT record in its zone through
//     dns's api, since §21.7's probation publishes no address record for a new network, and the dns edge answers it
//     signed over UDP; the second network's owner is refused the same name, and the edge still answers the first's;
//   - ledgers: the metadata cluster's write budget holds the evs block's spend for the writes, with no violation on
//     api's side;
//   - chaos: evs stopped mid-run and started again on its stores, after which every write reads back as it was
//     answered, through the restarted block;
//   - the quota ledgers: evs's drain at the restart sent its usage window through api's quota surface, so each
//     network's quota state holds the bytes its writes stored, and its transfer was drawn from its balance, never past
//     it.
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
	// The ACL document that grants the first network's members admin on the record's host, the host and the text its
	// TXT record holds, and the text the second network's refused write names: one character-string each, so no space,
	// which the API's presentation format reads as two.
	DnsAclPolicy   string
	DnsRecordHost  string
	DnsRecordText  string
	DnsForeignText string
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
		DnsAclPolicy:      `{"grants": [{"src": ["autogroup:member"], "dst": ["dns:smoke"], "allow": ["admin"]}]}`,
		DnsRecordHost:     "smoke",
		DnsRecordText:     "e2e-world-smoke",
		DnsForeignText:    "written-from-another-network",
	}
}

// One sidecar's write in the smoke's evs step: its path, its content and what its writer was answered.
type worldSmokeWrite struct {
	device  *sidecart3.T3Device
	path    string
	content []byte
	result  *sidecar.EvsWriteResult
}

// Every block, two networks of three sidecars, and one step through each service the world wires end to end. See the
// file's header.
func TestE2eWorldSmoke(t *testing.T) {
	settings := defaultWorldSmokeSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		worldSettings := DefaultWorldSettings()
		worldSettings.NetworkCount = settings.NetworkCount
		worldSettings.ClientsPerNetwork = settings.ClientsPerNetwork
		world := NewWorld(t, worldSettings)
		defer world.Close()

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
		if 0 < len(ledgers.BudgetViolations) {
			t.Fatalf("api's budget saw violations: %v", ledgers.BudgetViolations)
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
		if _, err := world.SaveAclPolicy(ctx, first, first.Owner(), settings.DnsAclPolicy); err != nil {
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
		}
	})
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
