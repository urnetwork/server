package controller

// GET /network/provider-status with its query (client_id, format) without a
// database: which client a query answers for, that every client id the caller
// may not read gets one answer, the compact form, total_clients in the
// network's answer only, the scope of the answer's entity tag, and that the
// answer without a query is the network's answer, compatible with the one
// before. The reads are faked.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

var providerStatusQueryTestNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// The codes of the reasons the fake statuses give, as the model codes them.
var testingQueryReasonCodes = map[string]string{
	model.ProviderStatusReasonEgressWarmingUp: model.ProviderStatusCodeWarmingUp,
	model.ProviderStatusReasonNone:            model.ProviderStatusCodeNone,
	model.ProviderStatusReasonNotProviding:    model.ProviderStatusCodeNotProviding,
}

// A decided status with every text, shaped as the model gives one: numbers
// with and without values, a counted number and a country.
func testingQueryProviderStatus(clientId server.Id, reason string) *model.ProviderStatus {
	measuredAt := providerStatusQueryTestNow.Add(-time.Hour)
	return &model.ProviderStatus{
		ClientId:   clientId,
		Reason:     reason,
		ReasonCode: testingQueryReasonCodes[reason],
		ReasonText: "Building network check history. The checks on this connection are passing so far, but it's too early to be sure.",
		Admission: &model.ProviderAdmission{
			Connected:        true,
			LocationValid:    true,
			ProvidePublic:    true,
			ReliabilityOk:    true,
			SpeedTestDone:    true,
			Egress:           model.ProviderEgressPass,
			EgressMeasuredAt: &measuredAt,
		},
		Ranking: []*model.ProviderRankingNumber{
			{
				Name:        model.ProviderStatusNumberReliability5m,
				HasValue:    true,
				Value:       1,
				Passes:      true,
				Explanation: "How steadily this device stayed connected over the last 5 minutes: 1.00. It scales how often clients are offered this device; staying connected raises it.",
			},
			{
				Name:        model.ProviderStatusNumberReliability12h,
				HasMinimum:  true,
				Minimum:     0.7,
				Passes:      true,
				Explanation: "No reliability history for the last 12 hours yet. Missing history doesn't count against this device.",
			},
			{
				Name:        model.ProviderStatusNumberUrlChecks,
				HasValue:    true,
				Value:       0.9,
				HasMinimum:  true,
				Minimum:     0.8,
				Passes:      true,
				Count:       18,
				Total:       20,
				Explanation: "Test sites that loaded through this connection in the last 8 hours: 18 of 20. At least 4 in 5 must load; filtering by your internet provider lowers it.",
			},
			{
				Name:        model.ProviderStatusNumberLatency,
				HasValue:    true,
				Value:       0,
				HasMaximum:  true,
				Maximum:     200,
				Passes:      true,
				Explanation: "Delay measured above what is expected for this location: 0 ms. Lower delay is offered more often; above 200 ms clients prefer other providers.",
			},
		},
		Country: &model.ProviderStatusCountry{
			CountryCode:         "us",
			Country:             "United States",
			ObservedCountryCode: "us",
			Explanation:         "Clients who choose United States can be offered this device.",
		},
		EvaluateTime: providerStatusQueryTestNow,
	}
}

// One client id the fake reads know: its network and whether it is a
// provider client.
type providerStatusQueryTestClient struct {
	networkId server.Id
	provider  bool
}

// The reads of an answer, faked, with what each was asked.
type providerStatusQueryTestReads struct {
	clients map[server.Id]providerStatusQueryTestClient
	// the network's answer, what GetProviderStatus returns
	networkResult  *GetProviderStatusResult
	networkReads   int
	clientReads    []server.Id
	statusReads    []server.Id
	histogramReads [][]server.Id
}

// Replaces the reads of an answer until the test ends.
func newProviderStatusQueryTestReads(t *testing.T) *providerStatusQueryTestReads {
	t.Helper()
	reads := &providerStatusQueryTestReads{
		clients: map[server.Id]providerStatusQueryTestClient{},
	}

	savedNetwork := getNetworkProviderStatus
	savedClient := getProviderStatusClient
	savedStatus := getCachedClientProviderStatus
	savedHistograms := getProviderAppearanceHistograms
	t.Cleanup(func() {
		getNetworkProviderStatus = savedNetwork
		getProviderStatusClient = savedClient
		getCachedClientProviderStatus = savedStatus
		getProviderAppearanceHistograms = savedHistograms
	})

	getNetworkProviderStatus = func(clientSession *session.ClientSession) (*GetProviderStatusResult, error) {
		reads.networkReads += 1
		return reads.networkResult, nil
	}
	// as model.GetProviderStatusClient reads it: a client of the network
	// asked about, and whether it is a provider client
	getProviderStatusClient = func(ctx context.Context, networkId server.Id, clientId server.Id) (bool, bool) {
		reads.clientReads = append(reads.clientReads, clientId)
		client, ok := reads.clients[clientId]
		if !ok || client.networkId != networkId {
			return false, false
		}
		return true, client.provider
	}
	// as model.GetClientProviderStatus decides: any client of the caller's
	// network
	getCachedClientProviderStatus = func(clientSession *session.ClientSession, clientId server.Id) (*model.ProviderStatusesResult, error) {
		reads.statusReads = append(reads.statusReads, clientId)
		result := &model.ProviderStatusesResult{
			Providers: []*model.ProviderStatus{},
		}
		if client, ok := reads.clients[clientId]; ok && client.networkId == clientSession.ByJwt.NetworkId {
			reason := model.ProviderStatusReasonEgressWarmingUp
			if !client.provider {
				reason = model.ProviderStatusReasonNotProviding
			}
			result.Providers = append(result.Providers, testingQueryProviderStatus(clientId, reason))
		}
		return result, nil
	}
	getProviderAppearanceHistograms = func(ctx context.Context, clientIds []server.Id, now time.Time) (map[server.Id]*model.ProviderAppearanceHistogram, error) {
		reads.histogramReads = append(reads.histogramReads, slices.Clone(clientIds))
		histograms := map[server.Id]*model.ProviderAppearanceHistogram{}
		for _, clientId := range clientIds {
			histograms[clientId] = &model.ProviderAppearanceHistogram{
				StartMinute:          now.Unix()/60 - 59,
				BucketSeconds:        60,
				AppearancesPerMinute: []int64{3, 0, 1},
			}
		}
		return histograms, nil
	}
	return reads
}

// A session of networkId: the client token of callerClientId, or the network
// credential when it is nil.
func testingProviderStatusQuerySession(networkId server.Id, callerClientId *server.Id) *session.ClientSession {
	byJwt := &session.ByJwt{
		NetworkId: networkId,
		UserId:    server.NewId(),
	}
	if callerClientId != nil {
		deviceId := server.NewId()
		byJwt.DeviceId = &deviceId
		byJwt.ClientId = callerClientId
	}
	return session.Testing_CreateClientSession(context.Background(), byJwt)
}

func testingProviderStatusQueryJson(t *testing.T, value any) string {
	t.Helper()
	valueJson, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(valueJson)
}

// The one network of a query test: the caller's client and another provider
// client of its network, a client of the network that is not a provider, and
// another network's provider client.
type providerStatusQueryTestNetwork struct {
	networkId        server.Id
	callerClientId   server.Id
	providerClientId server.Id
	idleClientId     server.Id
	otherNetworkId   server.Id
	otherClientId    server.Id
}

func newProviderStatusQueryTestNetwork(reads *providerStatusQueryTestReads) *providerStatusQueryTestNetwork {
	network := &providerStatusQueryTestNetwork{
		networkId:        server.NewId(),
		callerClientId:   server.NewId(),
		providerClientId: server.NewId(),
		idleClientId:     server.NewId(),
		otherNetworkId:   server.NewId(),
		otherClientId:    server.NewId(),
	}
	reads.clients[network.callerClientId] = providerStatusQueryTestClient{networkId: network.networkId, provider: true}
	reads.clients[network.providerClientId] = providerStatusQueryTestClient{networkId: network.networkId, provider: true}
	reads.clients[network.idleClientId] = providerStatusQueryTestClient{networkId: network.networkId, provider: false}
	reads.clients[network.otherClientId] = providerStatusQueryTestClient{networkId: network.otherNetworkId, provider: true}
	return network
}

// client_id answers the status of the caller's own client: that one client,
// decided for the caller's network, with its appearances read fresh. The
// caller's own client is read even when it is not a provider, as the
// network's answer adds it when truncated; another provider client of the
// network, which a network past the 32-client cap leaves out of its answer,
// is read with the client token and with the network credential alike.
func TestProviderStatusQueryAnswersTheCallersClient(t *testing.T) {
	reads := newProviderStatusQueryTestReads(t)
	network := newProviderStatusQueryTestNetwork(reads)
	callerSession := testingProviderStatusQuerySession(network.networkId, &network.callerClientId)

	result, err := GetProviderStatusWithArgs(&GetProviderStatusArgs{ClientId: network.callerClientId.String()}, callerSession)
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != nil || len(result.Providers) != 1 || result.Truncated {
		t.Fatalf("result = %s", testingProviderStatusQueryJson(t, result))
	}
	status := result.Providers[0]
	if status.ClientId != network.callerClientId || status.Reason != model.ProviderStatusReasonEgressWarmingUp || status.ReasonText == "" {
		t.Fatalf("status = %s", testingProviderStatusQueryJson(t, status))
	}
	if status.Appearances == nil || !slices.Equal(status.Appearances.AppearancesPerMinute, []int64{3, 0, 1}) {
		t.Fatalf("appearances = %+v, want the fresh read", status.Appearances)
	}
	if reads.networkReads != 0 {
		t.Fatalf("a query for one client decided the whole network %d times", reads.networkReads)
	}
	if !slices.Equal(reads.statusReads, []server.Id{network.callerClientId}) || len(reads.histogramReads) != 1 || !slices.Equal(reads.histogramReads[0], []server.Id{network.callerClientId}) {
		t.Fatalf("status reads %v, histogram reads %v", reads.statusReads, reads.histogramReads)
	}

	// the caller's own client when it is not a provider
	idleSession := testingProviderStatusQuerySession(network.networkId, &network.idleClientId)
	result, err = GetProviderStatusWithArgs(&GetProviderStatusArgs{ClientId: network.idleClientId.String()}, idleSession)
	if err != nil {
		t.Fatal(err)
	}
	if result.Error != nil || len(result.Providers) != 1 || result.Providers[0].ClientId != network.idleClientId || result.Providers[0].Reason != model.ProviderStatusReasonNotProviding {
		t.Fatalf("own client that is not a provider: %s", testingProviderStatusQueryJson(t, result))
	}

	// another provider client of the caller's network
	networkSession := testingProviderStatusQuerySession(network.networkId, nil)
	for _, clientSession := range []*session.ClientSession{callerSession, networkSession} {
		result, err = GetProviderStatusWithArgs(&GetProviderStatusArgs{ClientId: network.providerClientId.String()}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if result.Error != nil || len(result.Providers) != 1 || result.Providers[0].ClientId != network.providerClientId {
			t.Fatalf("a provider client of the network: %s", testingProviderStatusQueryJson(t, result))
		}
	}
}

// Another network's client id answers exactly what an unknown id answers,
// byte for byte, and neither decides a status nor reads appearances, so the
// answer cannot tell a client of another network from no client at all.
func TestProviderStatusQueryAnotherNetworksClientIsNotFound(t *testing.T) {
	reads := newProviderStatusQueryTestReads(t)
	network := newProviderStatusQueryTestNetwork(reads)

	for _, clientSession := range []*session.ClientSession{
		testingProviderStatusQuerySession(network.networkId, &network.callerClientId),
		testingProviderStatusQuerySession(network.networkId, nil),
	} {
		answer := func(clientId server.Id) (string, string) {
			full, err := GetProviderStatusWithArgs(&GetProviderStatusArgs{ClientId: clientId.String()}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			compact, err := GetProviderStatusCompact(&GetProviderStatusArgs{ClientId: clientId.String(), Format: ProviderStatusFormatCompact}, clientSession)
			if err != nil {
				t.Fatal(err)
			}
			return testingProviderStatusQueryJson(t, full), testingProviderStatusQueryJson(t, compact)
		}

		otherFull, otherCompact := answer(network.otherClientId)
		unknownFull, unknownCompact := answer(server.NewId())
		want := `{"providers":[],"error":{"message":"Provider client not found in this network."}}`
		if otherFull != want || otherCompact != want {
			t.Fatalf("another network's client answered %s and %s, want %s", otherFull, otherCompact, want)
		}
		if otherFull != unknownFull || otherCompact != unknownCompact {
			t.Fatalf("another network's client %s / %s, an unknown id %s / %s", otherFull, otherCompact, unknownFull, unknownCompact)
		}
	}
	if len(reads.statusReads) != 0 || len(reads.histogramReads) != 0 || reads.networkReads != 0 {
		t.Fatalf("a refused id read statuses %v, histograms %v, the network %d times", reads.statusReads, reads.histogramReads, reads.networkReads)
	}
}

// An unknown client id answers that the caller's network has no such provider
// client, the same answer a client of the network that is not a provider
// gets when it is not the caller's own. A malformed id is refused as invalid.
func TestProviderStatusQueryUnknownClientIsNotFound(t *testing.T) {
	reads := newProviderStatusQueryTestReads(t)
	network := newProviderStatusQueryTestNetwork(reads)
	callerSession := testingProviderStatusQuerySession(network.networkId, &network.callerClientId)

	unknown, err := GetProviderStatusWithArgs(&GetProviderStatusArgs{ClientId: server.NewId().String()}, callerSession)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.Providers) != 0 || unknown.Providers == nil || unknown.Error == nil || unknown.Error.Message != ProviderStatusClientNotFoundMessage {
		t.Fatalf("unknown id: %s", testingProviderStatusQueryJson(t, unknown))
	}

	idle, err := GetProviderStatusWithArgs(&GetProviderStatusArgs{ClientId: network.idleClientId.String()}, callerSession)
	if err != nil {
		t.Fatal(err)
	}
	if testingProviderStatusQueryJson(t, idle) != testingProviderStatusQueryJson(t, unknown) {
		t.Fatalf("a client of the network that is not a provider %s, an unknown id %s", testingProviderStatusQueryJson(t, idle), testingProviderStatusQueryJson(t, unknown))
	}

	for _, clientId := range []string{"not-a-client-id", "00000000-0000-0000-0000"} {
		invalid, err := GetProviderStatusWithArgs(&GetProviderStatusArgs{ClientId: clientId}, callerSession)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"providers":[],"error":{"message":"Invalid client_id."}}`; testingProviderStatusQueryJson(t, invalid) != want {
			t.Fatalf("%q: %s, want %s", clientId, testingProviderStatusQueryJson(t, invalid), want)
		}
	}
	if len(reads.statusReads) != 0 || len(reads.histogramReads) != 0 || reads.networkReads != 0 {
		t.Fatalf("a refused id read statuses %v, histograms %v, the network %d times", reads.statusReads, reads.histogramReads, reads.networkReads)
	}
}

// format=compact answers the statuses of the full form, in the same order,
// with the same truncation and total_clients and each reason's code, without
// the reason text and explanations: a number keeps its name and whether it
// passes, gives a value, minimum or maximum only when it has one, and a count
// and total only when counted. It applies to the network's answer and to one
// client's.
func TestProviderStatusCompactForm(t *testing.T) {
	reads := newProviderStatusQueryTestReads(t)
	network := newProviderStatusQueryTestNetwork(reads)
	callerSession := testingProviderStatusQuerySession(network.networkId, &network.callerClientId)
	totalClients := 40
	reads.networkResult = &GetProviderStatusResult{
		Providers: []*model.ProviderStatus{
			testingQueryProviderStatus(network.callerClientId, model.ProviderStatusReasonEgressWarmingUp),
			testingQueryProviderStatus(network.providerClientId, model.ProviderStatusReasonNone),
		},
		Truncated:    true,
		TotalClients: &totalClients,
	}
	fullJson := testingProviderStatusQueryJson(t, reads.networkResult)

	compact, err := GetProviderStatusCompact(&GetProviderStatusArgs{Format: ProviderStatusFormatCompact}, callerSession)
	if err != nil {
		t.Fatal(err)
	}
	compactJson := testingProviderStatusQueryJson(t, compact)
	if !compact.Truncated || compact.Error != nil || len(compact.Providers) != 2 ||
		compact.Providers[0].ClientId != network.callerClientId || compact.Providers[1].ClientId != network.providerClientId {
		t.Fatalf("compact = %s", compactJson)
	}
	if compact.TotalClients == nil || *compact.TotalClients != totalClients ||
		compact.Providers[0].ReasonCode != model.ProviderStatusCodeWarmingUp || compact.Providers[1].ReasonCode != model.ProviderStatusCodeNone {
		t.Fatalf("compact total_clients and codes = %s", compactJson)
	}
	for _, key := range []string{`"reason_text"`, `"explanation"`, `"has_value"`, `"has_minimum"`, `"has_maximum"`} {
		if strings.Contains(compactJson, key) {
			t.Fatalf("the compact form carries %s: %s", key, compactJson)
		}
	}
	if fullJson != testingProviderStatusQueryJson(t, reads.networkResult) {
		t.Fatal("the compact form changed the full statuses")
	}

	status := compact.Providers[0]
	if status.Reason != model.ProviderStatusReasonEgressWarmingUp || status.Admission == nil || !status.Admission.Connected || !status.EvaluateTime.Equal(providerStatusQueryTestNow) {
		t.Fatalf("status = %s", testingProviderStatusQueryJson(t, status))
	}
	wantRanking := `[` +
		`{"name":"reliability_5m","value":1,"passes":true},` +
		`{"name":"reliability_12h","minimum":0.7,"passes":true},` +
		`{"name":"url_checks","value":0.9,"minimum":0.8,"passes":true,"count":18,"total":20},` +
		`{"name":"latency","value":0,"maximum":200,"passes":true}` +
		`]`
	if rankingJson := testingProviderStatusQueryJson(t, status.Ranking); rankingJson != wantRanking {
		t.Fatalf("ranking = %s, want %s", rankingJson, wantRanking)
	}
	if countryJson := testingProviderStatusQueryJson(t, status.Country); countryJson != `{"country_code":"us","country":"United States","observed_country_code":"us"}` {
		t.Fatalf("country = %s", countryJson)
	}
	if 2*len(compactJson) >= len(fullJson) {
		t.Fatalf("the compact form is %d bytes, the full %d", len(compactJson), len(fullJson))
	}

	// one client in the compact form
	one, err := GetProviderStatusCompact(&GetProviderStatusArgs{ClientId: network.providerClientId.String(), Format: ProviderStatusFormatCompact}, callerSession)
	if err != nil {
		t.Fatal(err)
	}
	oneJson := testingProviderStatusQueryJson(t, one)
	if one.Error != nil || one.Truncated || one.TotalClients != nil || len(one.Providers) != 1 || one.Providers[0].ClientId != network.providerClientId || one.Providers[0].Appearances == nil {
		t.Fatalf("one client = %s", oneJson)
	}
	if strings.Contains(oneJson, `"explanation"`) || strings.Contains(oneJson, `"reason_text"`) {
		t.Fatalf("one client in the compact form carries a text: %s", oneJson)
	}
}

// The answer without a query is the network's answer, compatible with the
// answer before the query and the codes existed: every field it had reads as
// it did, and the only fields added are total_clients beside truncated and
// reason_code in each status; no error, and every text. format=full answers it
// too, and another format is refused rather than read as full.
func TestProviderStatusDefaultAnswerCompatible(t *testing.T) {
	reads := newProviderStatusQueryTestReads(t)
	network := newProviderStatusQueryTestNetwork(reads)
	callerSession := testingProviderStatusQuerySession(network.networkId, &network.callerClientId)

	// a status as it was before reason_code, field for field
	type legacyProviderStatus struct {
		ClientId     server.Id                          `json:"client_id"`
		Reason       string                             `json:"reason"`
		ReasonText   string                             `json:"reason_text"`
		Admission    *model.ProviderAdmission           `json:"admission"`
		Ranking      []*model.ProviderRankingNumber     `json:"ranking"`
		Country      *model.ProviderStatusCountry       `json:"country,omitempty"`
		EvaluateTime time.Time                          `json:"evaluate_time"`
		Appearances  *model.ProviderAppearanceHistogram `json:"appearances,omitempty"`
	}
	// the answer as it was before the query, field for field
	type legacyGetProviderStatusResult struct {
		Providers []*legacyProviderStatus `json:"providers"`
		Truncated bool                    `json:"truncated,omitempty"`
	}
	for _, truncated := range []bool{false, true} {
		providers := []*model.ProviderStatus{
			testingQueryProviderStatus(network.callerClientId, model.ProviderStatusReasonEgressWarmingUp),
			testingQueryProviderStatus(network.providerClientId, model.ProviderStatusReasonNone),
		}
		totalClients := len(providers)
		if truncated {
			totalClients = 40
		}
		reads.networkResult = &GetProviderStatusResult{
			Providers:    providers,
			Truncated:    truncated,
			TotalClients: &totalClients,
		}
		legacy := &legacyGetProviderStatusResult{
			Truncated: truncated,
		}
		for _, status := range providers {
			legacy.Providers = append(legacy.Providers, &legacyProviderStatus{
				ClientId:     status.ClientId,
				Reason:       status.Reason,
				ReasonText:   status.ReasonText,
				Admission:    status.Admission,
				Ranking:      status.Ranking,
				Country:      status.Country,
				EvaluateTime: status.EvaluateTime,
				Appearances:  status.Appearances,
			})
		}
		var legacyAnswer any
		if err := json.Unmarshal([]byte(testingProviderStatusQueryJson(t, legacy)), &legacyAnswer); err != nil {
			t.Fatal(err)
		}

		for _, args := range []*GetProviderStatusArgs{{}, {Format: ProviderStatusFormatFull}} {
			networkReads := reads.networkReads
			result, err := GetProviderStatusWithArgs(args, callerSession)
			if err != nil {
				t.Fatal(err)
			}
			if result != reads.networkResult || reads.networkReads != networkReads+1 {
				t.Fatalf("%+v: not the network's answer", args)
			}
			resultJson := testingProviderStatusQueryJson(t, result)
			if strings.Contains(resultJson, `"error"`) || strings.Count(resultJson, `"reason_text"`) != 2 || strings.Count(resultJson, `"explanation"`) != 10 {
				t.Fatalf("%+v: %s", args, resultJson)
			}

			// the additive fields, with their values, and then exactly the
			// answer from before them
			var answer map[string]any
			if err := json.Unmarshal([]byte(resultJson), &answer); err != nil {
				t.Fatal(err)
			}
			if answer["total_clients"] != float64(totalClients) {
				t.Fatalf("%+v: total_clients %v, want %d", args, answer["total_clients"], totalClients)
			}
			delete(answer, "total_clients")
			for i, provider := range answer["providers"].([]any) {
				status := provider.(map[string]any)
				if status["reason_code"] != testingQueryReasonCodes[providers[i].Reason] {
					t.Fatalf("%+v: status %d reason_code %v", args, i, status["reason_code"])
				}
				delete(status, "reason_code")
			}
			if !reflect.DeepEqual(any(answer), legacyAnswer) {
				t.Fatalf("%+v: without the additive fields %v, before them %v", args, answer, legacyAnswer)
			}
		}
	}
	if len(reads.clientReads) != 0 || len(reads.statusReads) != 0 {
		t.Fatalf("the network's answer read clients %v, statuses %v", reads.clientReads, reads.statusReads)
	}

	networkReads := reads.networkReads
	for _, format := range []string{"xml", "Compact", "FULL"} {
		refused, err := GetProviderStatusWithArgs(&GetProviderStatusArgs{Format: format}, callerSession)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"providers":[],"error":{"message":"Invalid format."}}`; testingProviderStatusQueryJson(t, refused) != want {
			t.Fatalf("format %q: %s, want %s", format, testingProviderStatusQueryJson(t, refused), want)
		}
	}
	if reads.networkReads != networkReads {
		t.Fatal("an invalid format answered the network's statuses")
	}
}

// total_clients is in the network's answer only, in both forms: one client's
// answer and a refused query have none, and a network without providers
// counts 0.
func TestProviderStatusTotalClientsOnlyInTheNetworksAnswer(t *testing.T) {
	reads := newProviderStatusQueryTestReads(t)
	network := newProviderStatusQueryTestNetwork(reads)
	callerSession := testingProviderStatusQuerySession(network.networkId, &network.callerClientId)

	for _, totalClients := range []int{0, 2, 40} {
		total := totalClients
		reads.networkResult = &GetProviderStatusResult{
			Providers:    []*model.ProviderStatus{},
			Truncated:    model.ProviderStatusMaxClients < totalClients,
			TotalClients: &total,
		}
		full, err := GetProviderStatusWithArgs(&GetProviderStatusArgs{}, callerSession)
		if err != nil {
			t.Fatal(err)
		}
		compact, err := GetProviderStatusCompact(&GetProviderStatusArgs{Format: ProviderStatusFormatCompact}, callerSession)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf(`"total_clients":%d`, totalClients)
		if fullJson, compactJson := testingProviderStatusQueryJson(t, full), testingProviderStatusQueryJson(t, compact); !strings.Contains(fullJson, want) || !strings.Contains(compactJson, want) {
			t.Fatalf("total %d: full %s, compact %s", totalClients, fullJson, compactJson)
		}
	}

	for name, args := range map[string]*GetProviderStatusArgs{
		"one client":         {ClientId: network.providerClientId.String()},
		"one client compact": {ClientId: network.providerClientId.String(), Format: ProviderStatusFormatCompact},
		"not found":          {ClientId: server.NewId().String()},
		"invalid client_id":  {ClientId: "not-a-client-id"},
		"invalid format":     {Format: "xml"},
	} {
		var answer any
		var err error
		if args.Format == ProviderStatusFormatCompact {
			answer, err = GetProviderStatusCompact(args, callerSession)
		} else {
			answer, err = GetProviderStatusWithArgs(args, callerSession)
		}
		if err != nil {
			t.Fatal(err)
		}
		if answerJson := testingProviderStatusQueryJson(t, answer); strings.Contains(answerJson, "total_clients") {
			t.Fatalf("%s: %s", name, answerJson)
		}
	}
}

// The scope of an answer's entity tag is the query, canonical: spellings of
// one client id and no format or format=full share one, and every other
// query has its own; a value that is not valid is kept, escaped.
func TestProviderStatusEntityScope(t *testing.T) {
	clientId := server.NewId()
	scope := func(clientId string, format string) string {
		return ProviderStatusEntityScope(&GetProviderStatusArgs{ClientId: clientId, Format: format})
	}

	if network, full := scope("", ""), scope("", ProviderStatusFormatFull); network != full || network != "client_id=&format=full" {
		t.Fatalf("no format %q, format=full %q", network, full)
	}
	if lower, upper := scope(clientId.String(), ""), scope(strings.ToUpper(clientId.String()), ProviderStatusFormatFull); lower != upper || lower != "client_id="+clientId.String()+"&format=full" {
		t.Fatalf("one client id spelled two ways: %q, %q", lower, upper)
	}

	scopes := map[string]string{}
	for name, value := range map[string]string{
		"network":            scope("", ""),
		"network compact":    scope("", ProviderStatusFormatCompact),
		"one client":         scope(clientId.String(), ""),
		"one client compact": scope(clientId.String(), ProviderStatusFormatCompact),
		"another client":     scope(server.NewId().String(), ""),
		"invalid client_id":  scope("not a client&format=compact", ""),
		"invalid format":     scope("", "FULL"),
	} {
		if other, ok := scopes[value]; ok {
			t.Fatalf("%s and %s share the scope %q", name, other, value)
		}
		scopes[value] = name
	}
	if invalid := scope("not a client&format=compact", ""); invalid != "client_id=not+a+client%26format%3Dcompact&format=full" {
		t.Fatalf("an invalid client_id: %q", invalid)
	}
}

// The caller reads a client of its network that is a provider client, and its
// own client; nothing else.
func TestProviderStatusClientReadable(t *testing.T) {
	clientId := server.NewId()
	otherClientId := server.NewId()
	for _, testCase := range []struct {
		client, provider bool
		callerClientId   *server.Id
		want             bool
	}{
		{client: true, provider: true, callerClientId: nil, want: true},
		{client: true, provider: true, callerClientId: &otherClientId, want: true},
		{client: true, provider: false, callerClientId: &clientId, want: true},
		{client: true, provider: false, callerClientId: &otherClientId, want: false},
		{client: true, provider: false, callerClientId: nil, want: false},
		{client: false, provider: false, callerClientId: &clientId, want: false},
		{client: false, provider: false, callerClientId: nil, want: false},
	} {
		if got := providerStatusClientReadable(testCase.client, testCase.provider, clientId, testCase.callerClientId); got != testCase.want {
			t.Fatalf("%+v: %t", testCase, got)
		}
	}
}
