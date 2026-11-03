// Synthetic controls for the demand deposit's signed price, usage statement and
// deposit hotkey over the deterministic world.
package monitor

import (
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/demand/pricesheet"
	"github.com/urfoundation/sn/demand/usage"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"

	"github.com/urnetwork/server/startifact"
)

// One rao per byte, in rao per GiB.
const subnetDemandRaoPerGiB = uint64(1 << 30)

type subnetDemandPrice struct {
	epoch, gib, user, capRao uint64
}

func subnetDemandRaoText(rao uint64) string {
	return fmt.Sprintf("%d.%09d", rao/1_000_000_000, rao%1_000_000_000)
}

// A published sheet whose entries the key signed.
func subnetDemandSheetFile(t testing.TB, world *subnetWorld, key *ecdsa.PrivateKey, prices ...subnetDemandPrice) []byte {
	t.Helper()
	var file strings.Builder
	fmt.Fprintf(&file, "sn: %d\n", world.deployment.Netuid)
	if len(prices) != 0 {
		file.WriteString("signed_schedule:\n")
	}
	for _, price := range prices {
		entry := pricesheet.Entry{EffectiveEpoch: price.epoch, EpochCapRao: price.capRao, Tiers: []protocol.DepositTier{{RateNumeratorRaoPerGiB: price.gib, RateNumeratorRaoPerUser: price.user, RateDenominator: 1}}}
		message, err := entry.Message(world.deployment.ChainId, world.deployment.Netuid)
		if err != nil {
			t.Fatal(err)
		}
		signature, err := crypto.Sign(crypto.Keccak256(fmt.Appendf(nil, "\x19Ethereum Signed Message:\n%d%s", len(message), message)), key)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&file, "  %d:\n    epoch_cap_alpha: %s\n    signature: \"0x%s\"\n    tiers:\n      0:\n        alpha_per_gib: %s\n        alpha_per_user: %s\n", price.epoch, subnetDemandRaoText(price.capRao), hex.EncodeToString(signature), subnetDemandRaoText(price.gib), subnetDemandRaoText(price.user))
	}
	return []byte(file.String())
}

func subnetDemandKey(t testing.TB, seed string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.ToECDSA(subnetWorldHash("demand-key/" + seed).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Pins the deployment to a signer, and publishes the sheet at the world's first origin.
// The returned setter says what the address serves: nil is a site that is down.
func subnetDemandPinSheet(t *testing.T, world *subnetWorld, key *ecdsa.PrivateKey) func([]byte) {
	t.Helper()
	signer := crypto.PubkeyToAddress(key.PublicKey)
	previousSigner, previousUrl := subnetPriceSheetSigner, subnetPriceSheetUrl
	subnetPriceSheetSigner = func(chain uint64, netuid uint16) (common.Address, bool) {
		return signer, chain == world.deployment.ChainId && netuid == world.deployment.Netuid
	}
	subnetPriceSheetUrl = subnetWorldApi1 + "/price.yml"
	t.Cleanup(func() { subnetPriceSheetSigner, subnetPriceSheetUrl = previousSigner, previousUrl })
	var served []byte
	world.routes["/price.yml"] = func(request *http.Request) *http.Response {
		if served == nil {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: request}
		}
		return subnetWorldJsonResponse(request, served)
	}
	return func(raw []byte) { served = raw }
}

// Each operator signs with its keyed artifact signer, and the reserve views follow the
// deposits, as in a priced world.
func subnetDemandPricedOperators(t *testing.T, world *subnetWorld) {
	t.Helper()
	for i := range world.settings.Operators {
		world.settings.Operators[i].ArtifactSigner = strings.ToLower(crypto.PubkeyToAddress(subnetWorldArtifactKey(t, world.settings.Operators[i].NoId).PublicKey).Hex())
	}
	world.contracts[subnetStakingPrecompile].views["getStake"] = func([]any) ([]any, error) {
		return world.contracts[world.sink].views["liveStake"](nil)
	}
}

func subnetDemandCount(world *subnetWorld, requested string) int {
	count := 0
	for _, request := range world.requests {
		if strings.HasSuffix(request, " "+requested) {
			count++
		}
	}
	return count
}

// Settlement v2 for every operator from firstEpoch.
func subnetDemandDeclareV2(t *testing.T, world *subnetWorld, firstEpoch uint64) *subnetWorldContract {
	t.Helper()
	vault := subnetWorldAddress("vault2")
	contract := world.contract(vault, subnetWorldAbi(t, stabi.STSettlementVault2MetaData.ParseABI))
	contract.views["pools"] = func(inputs []any) ([]any, error) {
		return []any{stabi.ISTSettlementVault2Pool{Hotkey: subnetWorldKey("vault2-pool"), FirstEpoch: firstEpoch, Registered: true}}, nil
	}
	world.deployment.VaultV2 = strings.ToLower(vault.Hex())
	world.settings.Deployment = world.deployment
	return contract
}

// The evidence routes of the operators' published usage statements.
type subnetDemandEvidence struct {
	world   *subnetWorld
	runs    map[string][]string
	content map[string][]byte
}

func subnetDemandInstallEvidence(world *subnetWorld) *subnetDemandEvidence {
	evidence := &subnetDemandEvidence{world: world, runs: map[string][]string{}, content: map[string][]byte{}}
	world.routes["/sn/evidence/history"] = func(request *http.Request) *http.Response {
		objects := []map[string]any{}
		for _, hash := range evidence.runs[request.URL.Query().Get("run_id")] {
			objects = append(objects, map[string]any{"key": path.Join("synthetic", "st", "v1", "evidence", "history", strings.TrimPrefix(hash, "sha256:")+".json"), "size": len(evidence.content[hash])})
		}
		encoded, _ := json.Marshal(map[string]any{"schema": "urnetwork-release-evidence-history-v1", "objects": objects, "more": false, "next_after": ""})
		return subnetWorldJsonResponse(request, encoded)
	}
	world.routes["/sn/evidence"] = func(request *http.Request) *http.Response {
		hash := request.URL.Query().Get("hash")
		raw, ok := evidence.content[hash]
		if !ok {
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: request}
		}
		response := subnetWorldJsonResponse(request, raw)
		response.Header.Set("ETag", `"`+hash+`"`)
		return response
	}
	return evidence
}

// Publishes one statement of an operator's epoch, signed by key.
func (self *subnetDemandEvidence) publish(t *testing.T, noId, epoch uint64, totalBytes, users uint64, chunks []string, key *ecdsa.PrivateKey) {
	t.Helper()
	world := self.world
	start, end := world.epochStartBlock(epoch), world.epochStartBlock(epoch+1)
	statement := &usage.Statement{
		DeploymentID: world.deployment.DeploymentId, ChainID: world.deployment.ChainId, GenesisHash: world.deployment.GenesisHash, Netuid: world.deployment.Netuid,
		Coordinator: world.coordinator, VaultV2: common.HexToAddress(world.deployment.VaultV2), PolicyHash: world.deployment.PolicyHash, NoID: noId, Epoch: epoch,
		Start: usage.Boundary{Number: start, Hash: strings.ToLower(world.evmHash(start).Hex())}, End: usage.Boundary{Number: end, Hash: strings.ToLower(world.evmHash(end).Hex())},
		ProviderChunks: chunks, ProviderRows: uint64(len(chunks)), TotalUsageBytes: totalBytes, TotalUsers: users,
	}
	if err := statement.Seal(key); err != nil {
		t.Fatal(err)
	}
	payload, err := statement.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	envelope := &startifact.EvidenceEnvelope{DeploymentID: world.deployment.DeploymentId, ChainID: world.deployment.ChainId, GenesisHash: world.deployment.GenesisHash, Netuid: world.deployment.Netuid, Kind: usage.EvidenceKind, RunID: usage.RunID(noId, epoch), CreatedAt: "2026-11-07T00:00:00Z", Payload: payload}
	if err := startifact.SignEvidence(envelope, key); err != nil {
		t.Fatal(err)
	}
	raw, err := startifact.EvidenceBytes(envelope)
	if err != nil {
		t.Fatal(err)
	}
	self.content[envelope.ContentHash] = raw
	self.runs[usage.RunID(noId, epoch)] = append(self.runs[usage.RunID(noId, epoch)], envelope.ContentHash)
}

// Keeps a world's clock and enrollment moving together.
func subnetDemandAdvance(world *subnetWorld, by time.Duration) {
	world.now = world.now.Add(by)
	world.settings.ReviewUntil = world.now.Add(time.Hour).Format(time.RFC3339Nano)
}

// With no pinned signer nothing reads a sheet, an unset price stays the reviewed
// document's, and zero-price output is what it was.
func TestSubnetDepositsWithNoPinnedSignerReadNoSheet(t *testing.T) {
	world := newSubnetWorld(t)
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	if subnetDemandCount(world, "/price.yml") != 0 {
		t.Fatal("the sheet was read")
	}
}

// A published sheet with no entries, as at launch, changes nothing either.
func TestSubnetDepositsLaunchSheetIsTheZeroPrice(t *testing.T) {
	world := newSubnetWorld(t)
	serve := subnetDemandPinSheet(t, world, subnetDemandKey(t, "signer"))
	serve([]byte(fmt.Sprintf("sn: %d\ntiered_per_block_demand_deposits:\n  0:\n    alpha_per_gib: 0\n    alpha_per_user: 0\n", world.deployment.Netuid)))
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	if subnetDemandCount(world, "/price.yml") != 1 {
		t.Fatal("the sheet was not read")
	}
}

// A signed entry in force prices the audit: the exact deposit is compliant, neither an
// under nor an over deposit is, and the reviewed document's zero price does not hide it.
func TestSubnetDepositsAuditAtTheSignedPrice(t *testing.T) {
	setup := func(t *testing.T) (*subnetWorld, map[uint64]*big.Int) {
		world := newSubnetWorld(t)
		key := subnetDemandKey(t, "signer")
		serve := subnetDemandPinSheet(t, world, key)
		serve(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch, subnetDemandRaoPerGiB, 2, 1_000_000_000}))
		subnetDemandPricedOperators(t, world)
		required := map[uint64]*big.Int{}
		for noId := uint64(1); noId <= 2; noId++ {
			world.commitArtifact(noId, world.currentEpoch-1, []uint64{1000 * noId, 2000})
			required[noId] = big.NewInt(int64(1000*noId + 2000))
			world.setDeposit(noId, required[noId])
		}
		return world, required
	}
	t.Run("exact", func(t *testing.T) {
		world, _ := setup(t)
		requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	})
	for name, delta := range map[string]int64{"under": -1, "over": 1} {
		t.Run(name, func(t *testing.T) {
			world, required := setup(t)
			world.setDeposit(1, new(big.Int).Add(required[1], big.NewInt(delta)))
			alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "", SeverityWarn)
			if !strings.Contains(alert.Observed, "state=deposit_mismatch") {
				t.Fatal(alert.Observed)
			}
			if len(subnetAlerts(alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-2")) != 0 {
				t.Fatal("a compliant operator was flagged")
			}
			world.requirePrivate(t, alerts)
		})
	}
	// The epoch before the entry is the reviewed document's zero price.
	t.Run("not yet in force", func(t *testing.T) {
		world := newSubnetWorld(t)
		key := subnetDemandKey(t, "signer")
		serve := subnetDemandPinSheet(t, world, key)
		serve(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch + 1, subnetDemandRaoPerGiB, 2, 1_000_000_000}))
		requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	})
	// A zero entry over a priced document is a zero price.
	t.Run("a zero entry over a priced document", func(t *testing.T) {
		world := newSubnetWorld(t)
		world.usePricedPolicy()
		key := subnetDemandKey(t, "signer")
		serve := subnetDemandPinSheet(t, world, key)
		serve(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch, 0, 0, 1_000_000_000}))
		requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	})
}

// A file that does not verify pages, and never prices the audit: with a verified copy
// the copy prices it, and with none the reviewed document does, as it does for validators
// that hold no sheet. At today's zero price the audits are then healthy.
func TestSubnetDepositsBadSheet(t *testing.T) {
	cases := map[string]func(t *testing.T, world *subnetWorld, key *ecdsa.PrivateKey) ([]byte, string){
		"forged": func(t *testing.T, world *subnetWorld, _ *ecdsa.PrivateKey) ([]byte, string) {
			return subnetDemandSheetFile(t, world, subnetDemandKey(t, "forger"), subnetDemandPrice{1, subnetDemandRaoPerGiB, 2, 1_000_000_000}), "sheet-unverified"
		},
		"garbage": func(*testing.T, *subnetWorld, *ecdsa.PrivateKey) ([]byte, string) {
			return []byte("not: [a sheet"), "sheet-unparseable"
		},
		"another subnet": func(t *testing.T, world *subnetWorld, key *ecdsa.PrivateKey) ([]byte, string) {
			return []byte(strings.Replace(string(subnetDemandSheetFile(t, world, key)), fmt.Sprintf("sn: %d", world.deployment.Netuid), "sn: 7", 1)), "sheet-unverified"
		},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			world := newSubnetWorld(t)
			key := subnetDemandKey(t, "signer")
			serve := subnetDemandPinSheet(t, world, key)
			raw, reason := bad(t, world, key)
			serve(raw)
			alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetPriceSheetClass, "subnet-deposits/price-sheet", "", SeverityPage)
			if !strings.Contains(alert.Observed, reason) {
				t.Fatal(alert.Observed)
			}
			if len(subnetAlerts(alerts, subnetOperatorDepositMismatchClass, "")) != 0 {
				t.Fatalf("the bad file reached the audit: %+v", alerts)
			}
			world.requirePrivate(t, alerts)
		})
	}
	t.Run("a verified copy prices the audit through a forged file", func(t *testing.T) {
		world := newSubnetWorld(t)
		key := subnetDemandKey(t, "signer")
		serve := subnetDemandPinSheet(t, world, key)
		serve(subnetDemandSheetFile(t, world, key))
		signal := NewSubnetDepositsSignal()
		settings := world.signalSettings()
		requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
		serve(subnetDemandSheetFile(t, world, subnetDemandKey(t, "forger"), subnetDemandPrice{1, subnetDemandRaoPerGiB, 2, 1_000_000_000}))
		alerts := runSubnetWorld(t, signal, settings)
		requireSubnetAlert(t, alerts, subnetPriceSheetClass, "subnet-deposits/price-sheet", "", SeverityPage)
		if len(subnetAlerts(alerts, subnetOperatorDepositMismatchClass, "")) != 0 {
			t.Fatalf("the forged price reached the audit: %+v", alerts)
		}
	})
	t.Run("a file that drops a signed entry", func(t *testing.T) {
		world := newSubnetWorld(t)
		key := subnetDemandKey(t, "signer")
		serve := subnetDemandPinSheet(t, world, key)
		serve(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch + 5, subnetDemandRaoPerGiB, 2, 1_000_000_000}))
		signal := NewSubnetDepositsSignal()
		settings := world.signalSettings()
		requireNoSubnetAlerts(t, runSubnetWorld(t, signal, settings))
		serve(subnetDemandSheetFile(t, world, key))
		alert := requireSubnetAlert(t, runSubnetWorld(t, signal, settings), subnetPriceSheetClass, "subnet-deposits/price-sheet", "", SeverityPage)
		if !strings.Contains(alert.Observed, "sheet-rollback") {
			t.Fatal(alert.Observed)
		}
	})
}

// A site that is down is unknown, and a warning once the last verified copy grows stale.
// Nothing is wrong with the price meanwhile: the copy keeps pricing the audit.
func TestSubnetDepositsStaleSheet(t *testing.T) {
	world := newSubnetWorld(t)
	key := subnetDemandKey(t, "signer")
	serve := subnetDemandPinSheet(t, world, key)
	serve(subnetDemandSheetFile(t, world, key))
	signal := NewSubnetDepositsSignal()
	requireNoSubnetAlerts(t, runSubnetWorld(t, signal, world.signalSettings()))
	serve(nil)
	subnetDemandAdvance(world, time.Hour)
	alerts := runSubnetWorld(t, signal, world.signalSettings())
	requireSubnetAlert(t, alerts, subnetPriceSheetClass, "subnet-deposits/price-sheet", "unobserved", SeverityWarn)
	if len(subnetAlerts(alerts, subnetOperatorDepositMismatchClass, "")) != 0 {
		t.Fatalf("%+v", alerts)
	}
	subnetDemandAdvance(world, subnetPriceSheetStaleAfter)
	alerts = runSubnetWorld(t, signal, world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetPriceSheetClass, "subnet-deposits/price-sheet", "stale", SeverityWarn)
	if !strings.Contains(alert.Observed, "state=stale") {
		t.Fatal(alert.Observed)
	}
	// Never verified and down, as on a host that cannot reach the published address: the
	// audit prices under the reviewed document, as validators with no sheet do, and only
	// the sheet's own finding says it cannot be read. At today's zero price that is all.
	fresh := newSubnetWorld(t)
	freshServe := subnetDemandPinSheet(t, fresh, key)
	freshServe(nil)
	alerts = runSubnetWorld(t, NewSubnetDepositsSignal(), fresh.signalSettings())
	requireSubnetAlert(t, alerts, subnetPriceSheetClass, "subnet-deposits/price-sheet", "unobserved", SeverityWarn)
	if len(alerts) != 1 {
		t.Fatalf("%+v", alerts)
	}
}

// The validators' deposit cap rule, where the chain family judges it: each version in
// every epoch it is in force, from the current one on, against the cap in force there,
// the signed entry's or the document's. A raise needs no price to be legal, and an entry
// is judged against the version in force at its own epoch.
func TestSubnetChainDepositCapRule(t *testing.T) {
	schedule := func(world *subnetWorld, epoch uint64, epochCap, campaignCap uint64) {
		previous := world.policyVersions[len(world.policyVersions)-1]
		next := previous
		next.EffectiveEpoch = epoch
		next.EffectiveBlock = previous.EffectiveBlock + (epoch-previous.EffectiveEpoch)*previous.EpochBlocks
		next.EpochDepositCapRao, next.CampaignDepositCapRao = new(big.Int).SetUint64(epochCap), new(big.Int).SetUint64(campaignCap)
		world.policyVersions = append(world.policyVersions, next)
	}
	publish := func(t *testing.T, world *subnetWorld, prices ...subnetDemandPrice) {
		key := subnetDemandKey(t, "signer")
		subnetDemandPinSheet(t, world, key)(subnetDemandSheetFile(t, world, key, prices...))
	}
	run := func(t *testing.T, world *subnetWorld) Alerts {
		return runSubnetWorld(t, NewSubnetChainSignal(), world.signalSettings())
	}
	stops := func(t *testing.T, alerts Alerts, epoch uint64) {
		t.Helper()
		alert := requireSubnetAlert(t, alerts, subnetPolicyScheduleClass, "subnet-chain/policy", "", SeverityPage)
		if !strings.Contains(alert.Observed, fmt.Sprintf("policy-deposit-caps-from-epoch-%d", epoch)) {
			t.Fatal(alert.Observed)
		}
	}
	world := newSubnetWorld(t)
	epochCap, campaignCap, now := world.policy.Deposit.EpochCapRaoPerOperator, world.policy.Deposit.TotalTestCampaignCapRao, world.currentEpoch
	price := func(epoch, capRao uint64) subnetDemandPrice {
		return subnetDemandPrice{epoch, subnetDemandRaoPerGiB, 2, capRao}
	}
	t.Run("raised caps with no price page nothing", func(t *testing.T) {
		world := newSubnetWorld(t)
		schedule(world, now+2, 2*epochCap, 2*campaignCap)
		requireNoSubnetAlerts(t, run(t, world))
	})
	t.Run("an entry and its raise in the same epoch page nothing", func(t *testing.T) {
		world := newSubnetWorld(t)
		schedule(world, now+2, 2*epochCap, campaignCap)
		publish(t, world, price(now+2, 2*epochCap))
		requireNoSubnetAlerts(t, run(t, world))
	})
	t.Run("an entry at the document's cap pages nothing", func(t *testing.T) {
		world := newSubnetWorld(t)
		publish(t, world, price(now+1, epochCap))
		requireNoSubnetAlerts(t, run(t, world))
	})
	t.Run("an entry above the cap pages at its epoch", func(t *testing.T) {
		world := newSubnetWorld(t)
		publish(t, world, price(now+1, epochCap+1))
		stops(t, run(t, world), now+1)
	})
	t.Run("an entry ahead of its raise pages at the entry's epoch", func(t *testing.T) {
		world := newSubnetWorld(t)
		schedule(world, now+3, 2*epochCap, campaignCap)
		publish(t, world, price(now+2, 2*epochCap))
		stops(t, run(t, world), now+2)
	})
	t.Run("an entry in force now above the current cap pages now", func(t *testing.T) {
		world := newSubnetWorld(t)
		publish(t, world, price(now, epochCap+1))
		stops(t, run(t, world), now)
	})
	t.Run("a scheduled cut below the document's cap pages at its epoch", func(t *testing.T) {
		world := newSubnetWorld(t)
		schedule(world, now+4, epochCap-1, campaignCap)
		stops(t, run(t, world), now+4)
	})
	t.Run("a cut below a later entry pages at the entry", func(t *testing.T) {
		world := newSubnetWorld(t)
		schedule(world, now+2, 2*epochCap, campaignCap)
		schedule(world, now+5, epochCap, campaignCap)
		publish(t, world, price(now+2, 2*epochCap), price(now+6, 2*epochCap))
		stops(t, run(t, world), now+5)
	})
	t.Run("a campaign cap under the version's epoch cap pages", func(t *testing.T) {
		world := newSubnetWorld(t)
		schedule(world, now+2, campaignCap+1, campaignCap)
		stops(t, run(t, world), now+2)
	})
	t.Run("a campaign cap under the document's pages", func(t *testing.T) {
		world := newSubnetWorld(t)
		schedule(world, now+2, epochCap, campaignCap-1)
		stops(t, run(t, world), now+2)
	})
	t.Run("a sheet that cannot be read judges the last that verified", func(t *testing.T) {
		world := newSubnetWorld(t)
		key := subnetDemandKey(t, "signer")
		serve := subnetDemandPinSheet(t, world, key)
		serve(subnetDemandSheetFile(t, world, key, price(now+1, epochCap+1)))
		signal := NewSubnetChainSignal()
		stops(t, runSubnetWorld(t, signal, world.signalSettings()), now+1)
		serve(nil)
		stops(t, runSubnetWorld(t, signal, world.signalSettings()), now+1)
	})
}

// The campaign cap is a lifetime limit: its headroom, in epochs at the latest required
// deposit summed over the operators, warns under 8 and pages under 2. Nothing is read at
// zero price, and the last requirement stands until the next is known.
func TestSubnetDepositsCampaignHeadroom(t *testing.T) {
	const required = 3020 + 6020
	priced := func(t *testing.T, epochsLeft int64) (*subnetWorld, *subnetDemandEvidence) {
		world, evidence := subnetDemandV2World(t)
		for noId := uint64(1); noId <= 2; noId++ {
			evidence.publish(t, noId, world.currentEpoch-1, 3000*noId, 10, subnetDemandChunks(fmt.Sprintf("chunk-%d", noId)), subnetWorldArtifactKey(t, noId))
			world.setDeposit(noId, big.NewInt(int64(3000*noId+20)))
		}
		world.campaignReserved = new(big.Int).Sub(new(big.Int).SetUint64(world.policy.Deposit.TotalTestCampaignCapRao), big.NewInt(epochsLeft*required))
		return world, evidence
	}
	for _, test := range []struct {
		epochsLeft int64
		severity   Severity
	}{{8, ""}, {7, SeverityWarn}, {2, SeverityWarn}, {1, SeverityPage}, {0, SeverityPage}} {
		t.Run(fmt.Sprintf("%d epochs", test.epochsLeft), func(t *testing.T) {
			world, _ := priced(t, test.epochsLeft)
			alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
			if test.severity == "" {
				requireNoSubnetAlerts(t, alerts)
				return
			}
			alert := requireSubnetAlert(t, alerts, subnetCampaignHeadroomClass, "subnet-deposits/campaign-headroom", "", test.severity)
			if !strings.Contains(alert.Observed, fmt.Sprintf("%d-epochs", test.epochsLeft)) || len(alerts) != 1 {
				t.Fatalf("%+v", alerts)
			}
		})
	}
	t.Run("the last requirement stands until the next is known", func(t *testing.T) {
		world, evidence := priced(t, 3)
		signal := NewSubnetDepositsSignal()
		requireSubnetAlert(t, runSubnetWorld(t, signal, world.signalSettings()), subnetCampaignHeadroomClass, "subnet-deposits/campaign-headroom", "", SeverityWarn)
		evidence.runs = map[string][]string{}
		requireSubnetAlert(t, runSubnetWorld(t, signal, world.signalSettings()), subnetCampaignHeadroomClass, "subnet-deposits/campaign-headroom", "", SeverityWarn)
	})
	t.Run("an undecided operator counts at its last requirement", func(t *testing.T) {
		world, evidence := priced(t, 3)
		signal := NewSubnetDepositsSignal()
		requireSubnetAlert(t, runSubnetWorld(t, signal, world.signalSettings()), subnetCampaignHeadroomClass, "subnet-deposits/campaign-headroom", "", SeverityWarn)
		// Operator 2's statement is not readable this run; operator 1 alone would
		// leave the headroom looking like 8 epochs.
		delete(evidence.runs, usage.RunID(2, world.currentEpoch-1))
		alert := requireSubnetAlert(t, runSubnetWorld(t, signal, world.signalSettings()), subnetCampaignHeadroomClass, "subnet-deposits/campaign-headroom", "", SeverityWarn)
		if !strings.Contains(alert.Observed, "3-epochs") {
			t.Fatal(alert.Observed)
		}
	})
	schedule := func(world *subnetWorld, epoch uint64, campaignCap uint64) {
		previous := world.policyVersions[len(world.policyVersions)-1]
		next := previous
		next.EffectiveEpoch = epoch
		next.EffectiveBlock = previous.EffectiveBlock + (epoch-previous.EffectiveEpoch)*previous.EpochBlocks
		next.CampaignDepositCapRao = new(big.Int).SetUint64(campaignCap)
		world.policyVersions = append(world.policyVersions, next)
	}
	t.Run("a raise scheduled in time counts", func(t *testing.T) {
		world, _ := priced(t, 3)
		schedule(world, world.currentEpoch+3, world.policy.Deposit.TotalTestCampaignCapRao+20*required)
		requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	})
	t.Run("a raise scheduled too late does not", func(t *testing.T) {
		world, _ := priced(t, 3)
		schedule(world, world.currentEpoch+4, world.policy.Deposit.TotalTestCampaignCapRao+20*required)
		alert := requireSubnetAlert(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()), subnetCampaignHeadroomClass, "subnet-deposits/campaign-headroom", "", SeverityWarn)
		if !strings.Contains(alert.Observed, "3-epochs") {
			t.Fatal(alert.Observed)
		}
	})
	t.Run("zero price reads nothing", func(t *testing.T) {
		world := newSubnetWorld(t)
		world.campaignReserved = new(big.Int).SetUint64(world.policy.Deposit.TotalTestCampaignCapRao)
		requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
		if world.campaignReservedReads != 0 {
			t.Fatalf("read campaignReserved %d times at zero price", world.campaignReservedReads)
		}
	})
}

// A world priced by the sheet from this epoch, whose source epoch settled under v2.
func subnetDemandV2World(t *testing.T) (*subnetWorld, *subnetDemandEvidence) {
	t.Helper()
	world := newSubnetWorld(t)
	key := subnetDemandKey(t, "signer")
	serve := subnetDemandPinSheet(t, world, key)
	serve(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch, subnetDemandRaoPerGiB, 2, 1_000_000_000}))
	subnetDemandPricedOperators(t, world)
	subnetDemandDeclareV2(t, world, world.currentEpoch-1)
	return world, subnetDemandInstallEvidence(world)
}

func subnetDemandChunks(seed string) []string {
	return []string{"sha256:" + strings.TrimPrefix(subnetWorldHash(seed).Hex(), "0x")}
}

// A v2 source is judged by the statement, at the signed price: its bytes and users, each
// priced. A v1 root on the epoch, which the phase C sweep commits, changes nothing.
func TestSubnetDepositsAuditAV2Source(t *testing.T) {
	world, evidence := subnetDemandV2World(t)
	for noId := uint64(1); noId <= 2; noId++ {
		evidence.publish(t, noId, world.currentEpoch-1, 3000*noId, 10, subnetDemandChunks(fmt.Sprintf("chunk-%d", noId)), subnetWorldArtifactKey(t, noId))
		world.setDeposit(noId, big.NewInt(int64(3000*noId+20)))
	}
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	if subnetDemandCount(world, "/sn/artifact") != 0 {
		t.Fatal("a v2 source read a payout artifact")
	}
	// The sweep's root changes nothing about which claim is judged.
	world.commitArtifact(1, world.currentEpoch-1, []uint64{1})
	world.setDeposit(1, big.NewInt(1))
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "", SeverityWarn)
	if !strings.Contains(alert.Observed, "state=deposit_mismatch") {
		t.Fatal(alert.Observed)
	}
	world.requirePrivate(t, alerts)
}

// A source epoch before the pool's first v2 epoch keeps the artifact.
func TestSubnetDepositsV1SourceUnderDeclaredV2(t *testing.T) {
	world, _ := subnetDemandV2World(t)
	subnetDemandDeclareV2(t, world, world.currentEpoch)
	for noId := uint64(1); noId <= 2; noId++ {
		world.commitArtifact(noId, world.currentEpoch-1, []uint64{1000 * noId, 2000})
		world.setDeposit(noId, big.NewInt(int64(1000*noId+2000)))
	}
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
	if subnetDemandCount(world, "/sn/evidence/history") != 0 {
		t.Fatal("a v1 source read a usage statement")
	}
}

// An unreadable pool leaves the audit unknown, never a v1 read of a v2 epoch.
func TestSubnetDepositsV2PoolUnavailableIsUnknown(t *testing.T) {
	world, _ := subnetDemandV2World(t)
	subnetDemandDeclareV2(t, world, world.currentEpoch-1).views["pools"] = func([]any) ([]any, error) {
		return nil, fmt.Errorf("revert")
	}
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "unobserved", SeverityWarn)
	if !strings.Contains(alert.Observed, "v2-pool-unavailable") {
		t.Fatal(alert.Observed)
	}
}

// A statement missing after the commit deadline pages while a price is in force, and is
// pending before it.
func TestSubnetUsageStatementMissing(t *testing.T) {
	world, _ := subnetDemandV2World(t)
	world.moveFinalized(world.epochStartBlock(world.currentEpoch) + 100)
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))

	world, _ = subnetDemandV2World(t)
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetUsageStatementClass, "subnet-deposits/no-1", "", SeverityPage)
	alert := requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "", SeverityWarn)
	if !strings.Contains(alert.Observed, "state=artifact_unavailable") {
		t.Fatal(alert.Observed)
	}
	requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/operators", "", SeverityPage)
	world.requirePrivate(t, alerts)
}

// Ahead of a scheduled price the same absence warns, because the first priced deposit
// will need the same pipeline, and nothing is owed yet.
func TestSubnetUsageStatementAheadOfAScheduledPrice(t *testing.T) {
	world := newSubnetWorld(t)
	key := subnetDemandKey(t, "signer")
	serve := subnetDemandPinSheet(t, world, key)
	serve(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch + 1, subnetDemandRaoPerGiB, 2, 1_000_000_000}))
	subnetDemandPricedOperators(t, world)
	subnetDemandDeclareV2(t, world, world.currentEpoch-1)
	evidence := subnetDemandInstallEvidence(world)
	alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetUsageStatementClass, "subnet-deposits/no-1", "", SeverityWarn)
	if alert.Severity != SeverityWarn || len(subnetAlerts(alerts, subnetOperatorDepositMismatchClass, "")) != 0 {
		t.Fatalf("%+v", alerts)
	}
	for noId := uint64(1); noId <= 2; noId++ {
		evidence.publish(t, noId, world.currentEpoch-1, 1, 1, subnetDemandChunks("chunk"), subnetWorldArtifactKey(t, noId))
	}
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()))
}

// With nothing scheduled, a zero price reads no pool and no statement at all.
func TestSubnetUsageStatementIsNotReadAtZeroPrice(t *testing.T) {
	idle := newSubnetWorld(t)
	subnetDemandDeclareV2(t, idle, idle.currentEpoch-1).views["pools"] = func([]any) ([]any, error) {
		t.Error("a pool was read at zero price with nothing scheduled")
		return nil, fmt.Errorf("revert")
	}
	subnetDemandInstallEvidence(idle)
	requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetDepositsSignal(), idle.signalSettings()))
	if subnetDemandCount(idle, "/sn/evidence/history") != 0 {
		t.Fatal("a statement was read at zero price with nothing scheduled")
	}
}

// Only a statement that verifies speaks for the operator.
func TestSubnetUsageStatementVerdicts(t *testing.T) {
	t.Run("another signer", func(t *testing.T) {
		world, evidence := subnetDemandV2World(t)
		evidence.publish(t, 1, world.currentEpoch-1, 1, 1, subnetDemandChunks("chunk"), subnetDemandKey(t, "another-signer"))
		alert := requireSubnetAlert(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()), subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "", SeverityWarn)
		if !strings.Contains(alert.Observed, "state="+"artifact_invalid") {
			t.Fatal(alert.Observed)
		}
	})
	t.Run("two totals for the same chunks equivocate", func(t *testing.T) {
		world, evidence := subnetDemandV2World(t)
		chunks := subnetDemandChunks("chunk")
		evidence.publish(t, 1, world.currentEpoch-1, 1, 1, chunks, subnetWorldArtifactKey(t, 1))
		evidence.publish(t, 1, world.currentEpoch-1, 2, 1, chunks, subnetWorldArtifactKey(t, 1))
		alert := requireSubnetAlert(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()), subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "", SeverityWarn)
		if !strings.Contains(alert.Observed, "state=artifact_equivocation") {
			t.Fatal(alert.Observed)
		}
	})
	t.Run("a rebuild's two statements are unknown here", func(t *testing.T) {
		world, evidence := subnetDemandV2World(t)
		evidence.publish(t, 1, world.currentEpoch-1, 1, 1, subnetDemandChunks("chunk"), subnetWorldArtifactKey(t, 1))
		evidence.publish(t, 1, world.currentEpoch-1, 2, 1, subnetDemandChunks("rebuilt"), subnetWorldArtifactKey(t, 1))
		alert := requireSubnetAlert(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()), subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "unobserved", SeverityWarn)
		if !strings.Contains(alert.Observed, "usage-statement-rebuilt") {
			t.Fatal(alert.Observed)
		}
	})
	t.Run("unreadable origins are unknown", func(t *testing.T) {
		world, _ := subnetDemandV2World(t)
		world.routes["/sn/evidence/history"] = func(request *http.Request) *http.Response {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: request}
		}
		alerts := runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings())
		requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "unobserved", SeverityWarn)
		requireSubnetAlert(t, alerts, subnetUsageStatementClass, "subnet-deposits/no-1", "unobserved", SeverityWarn)
	})
}

// An unpaid priced deposit warns as a mismatch from the start, and pages once it has
// stayed unpaid for hours after its totals exist. Measured from the root commit under
// v1, and from when the statement was first seen under v2.
func TestSubnetDepositNotLanding(t *testing.T) {
	t.Run("under v2", func(t *testing.T) {
		world, evidence := subnetDemandV2World(t)
		for noId := uint64(1); noId <= 2; noId++ {
			evidence.publish(t, noId, world.currentEpoch-1, 3000, 10, subnetDemandChunks("chunk"), subnetWorldArtifactKey(t, noId))
		}
		world.setDeposit(2, big.NewInt(3020))
		signal := NewSubnetDepositsSignal()
		settings := world.signalSettings()
		alerts := runSubnetWorld(t, signal, settings)
		requireSubnetAlert(t, alerts, subnetOperatorDepositMismatchClass, "subnet-deposits/no-1", "", SeverityWarn)
		if got := subnetAlerts(alerts, subnetDepositLandingClass, ""); len(got) != 0 {
			t.Fatalf("paged before the grace: %+v", got)
		}
		world.moveFinalized(world.finalized + subnetDepositLandingGraceBlocks)
		if got := subnetAlerts(runSubnetWorld(t, signal, settings), subnetDepositLandingClass, ""); len(got) != 0 {
			t.Fatalf("paged at the grace: %+v", got)
		}
		world.moveFinalized(world.finalized + 1)
		alerts = runSubnetWorld(t, signal, settings)
		requireSubnetAlert(t, alerts, subnetDepositLandingClass, "subnet-deposits/no-1", "", SeverityPage)
		if len(subnetAlerts(alerts, subnetDepositLandingClass, "subnet-deposits/no-2")) != 0 {
			t.Fatal("a paid operator paged")
		}
		world.requirePrivate(t, alerts)
	})
	t.Run("under v1", func(t *testing.T) {
		world := newSubnetWorld(t)
		key := subnetDemandKey(t, "signer")
		serve := subnetDemandPinSheet(t, world, key)
		serve(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch, subnetDemandRaoPerGiB, 2, 1_000_000_000}))
		subnetDemandPricedOperators(t, world)
		for noId := uint64(1); noId <= 2; noId++ {
			world.commitArtifact(noId, world.currentEpoch-1, []uint64{1000, 2000})
		}
		commit := world.epochStartBlock(world.currentEpoch) + 100
		world.moveFinalized(commit + subnetDepositLandingGraceBlocks)
		if got := subnetAlerts(runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()), subnetDepositLandingClass, ""); len(got) != 0 {
			t.Fatalf("paged at the grace: %+v", got)
		}
		world.moveFinalized(commit + subnetDepositLandingGraceBlocks + 1)
		requireSubnetAlert(t, runSubnetWorld(t, NewSubnetDepositsSignal(), world.signalSettings()), subnetDepositLandingClass, "subnet-deposits/no-1", "", SeverityPage)
	})
	// A wrong amount that landed is a mismatch, not a deposit that did not land.
	t.Run("a wrong amount is not unpaid", func(t *testing.T) {
		world, evidence := subnetDemandV2World(t)
		for noId := uint64(1); noId <= 2; noId++ {
			evidence.publish(t, noId, world.currentEpoch-1, 3000, 10, subnetDemandChunks("chunk"), subnetWorldArtifactKey(t, noId))
			world.setDeposit(noId, big.NewInt(1))
		}
		signal := NewSubnetDepositsSignal()
		settings := world.signalSettings()
		runSubnetWorld(t, signal, settings)
		world.moveFinalized(world.finalized + subnetDepositLandingGraceBlocks + 1)
		if got := subnetAlerts(runSubnetWorld(t, signal, settings), subnetDepositLandingClass, ""); len(got) != 0 {
			t.Fatalf("%+v", got)
		}
	})
}

func subnetDemandDepositHotkey(noId uint64) [32]byte {
	return subnetWorldKey(fmt.Sprintf("operator-%d-deposit", noId))
}

// The deposit hotkey's owner. The signer's mirror coldkey is healthy, however the
// association was sent, and no sheet is read to say so. A squat or swap pages at any
// price.
func TestSubnetIdentityDepositHotkeyOwner(t *testing.T) {
	t.Run("the signer's mirror", func(t *testing.T) {
		world := newSubnetWorld(t)
		subnetDemandPinSheet(t, world, subnetDemandKey(t, "signer"))
		requireNoSubnetAlerts(t, runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings()))
		if subnetDemandCount(world, "/price.yml") != 0 {
			t.Fatal("a healthy owner read the sheet")
		}
	})
	t.Run("another owner pages at zero price", func(t *testing.T) {
		world := newSubnetWorld(t)
		hotkey := subnetDemandDepositHotkey(1)
		world.setStorage("SubtensorModule", "Owner", types.AccountID(subnetWorldKey("squatter")), hotkey[:])
		alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
		alert := requireSubnetAlert(t, alerts, subnetDepositHotkeyOwnerClass, "subnet-identity/no-1", "", SeverityPage)
		if !strings.Contains(alert.Observed, "foreign-owner") || alert.Sustain != subnetProvedSustain {
			t.Fatal(alert.Observed)
		}
		if len(subnetAlerts(alerts, subnetDepositHotkeyOwnerClass, "subnet-identity/no-2")) != 0 {
			t.Fatal("another operator was flagged")
		}
		world.requirePrivate(t, alerts)
	})
	t.Run("the coordinator's rotation is followed", func(t *testing.T) {
		world := newSubnetWorld(t)
		hotkey := subnetDemandDepositHotkey(1)
		// The owner is the mirror of the signer the coordinator names, not of a
		// configured one: this mirror is of the wrong address.
		world.setStorage("SubtensorModule", "Owner", types.AccountID(subnetMirroredColdkey(subnetWorldAddress("operator-1-root-signer"))), hotkey[:])
		requireSubnetAlert(t, runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings()), subnetDepositHotkeyOwnerClass, "subnet-identity/no-1", "", SeverityPage)
	})
}

// Unset, every priced deposit would fail. That warns at zero price and pages once a
// priced entry is in force or scheduled, or the reviewed document itself is priced.
func TestSubnetIdentityDepositHotkeyUnset(t *testing.T) {
	unset := func(world *subnetWorld) {
		hotkey := subnetDemandDepositHotkey(1)
		world.deleteStorage("SubtensorModule", "Owner", hotkey[:])
	}
	requireOwner := func(t *testing.T, world *subnetWorld, severity Severity) Alert {
		t.Helper()
		alerts := runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings())
		alert := requireSubnetAlert(t, alerts, subnetDepositHotkeyOwnerClass, "subnet-identity/no-1", "", severity)
		if !strings.Contains(alert.Observed, "unset") {
			t.Fatal(alert.Observed)
		}
		world.requirePrivate(t, alerts)
		return alert
	}
	t.Run("zero price, no pinned signer", func(t *testing.T) {
		world := newSubnetWorld(t)
		unset(world)
		requireOwner(t, world, SeverityWarn)
	})
	t.Run("a zero value is unset", func(t *testing.T) {
		world := newSubnetWorld(t)
		hotkey := subnetDemandDepositHotkey(1)
		world.setStorage("SubtensorModule", "Owner", types.AccountID([32]byte{}), hotkey[:])
		requireOwner(t, world, SeverityWarn)
	})
	t.Run("zero price, a sheet with no entries", func(t *testing.T) {
		world := newSubnetWorld(t)
		unset(world)
		key := subnetDemandKey(t, "signer")
		subnetDemandPinSheet(t, world, key)(subnetDemandSheetFile(t, world, key))
		requireOwner(t, world, SeverityWarn)
	})
	t.Run("a priced entry in force", func(t *testing.T) {
		world := newSubnetWorld(t)
		unset(world)
		key := subnetDemandKey(t, "signer")
		subnetDemandPinSheet(t, world, key)(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch, subnetDemandRaoPerGiB, 2, 1_000_000_000}))
		requireOwner(t, world, SeverityPage)
	})
	t.Run("a priced entry scheduled", func(t *testing.T) {
		world := newSubnetWorld(t)
		unset(world)
		key := subnetDemandKey(t, "signer")
		subnetDemandPinSheet(t, world, key)(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch + 9, subnetDemandRaoPerGiB, 2, 1_000_000_000}))
		requireOwner(t, world, SeverityPage)
	})
	t.Run("a zero entry scheduled", func(t *testing.T) {
		world := newSubnetWorld(t)
		unset(world)
		key := subnetDemandKey(t, "signer")
		subnetDemandPinSheet(t, world, key)(subnetDemandSheetFile(t, world, key, subnetDemandPrice{world.currentEpoch + 9, 0, 0, 1_000_000_000}))
		requireOwner(t, world, SeverityWarn)
	})
	t.Run("a priced document", func(t *testing.T) {
		world := newSubnetWorld(t)
		world.usePricedPolicy()
		unset(world)
		requireOwner(t, world, SeverityPage)
	})
	t.Run("the sheet site down warns, never pages", func(t *testing.T) {
		world := newSubnetWorld(t)
		unset(world)
		subnetDemandPinSheet(t, world, subnetDemandKey(t, "signer"))
		requireOwner(t, world, SeverityWarn)
	})
}

// A deployment without the deposit hotkey has nothing to check, and an operator not
// active has no obligation.
func TestSubnetIdentityDepositHotkeyInert(t *testing.T) {
	world := newSubnetWorld(t)
	original := world.contracts[world.coordinator].views["operatorAt"]
	world.contracts[world.coordinator].views["operatorAt"] = func(inputs []any) ([]any, error) {
		values, err := original(inputs)
		version := values[0].(stabi.STCoordinatorOperatorVersion)
		version.DepositHotkey = [32]byte{}
		return []any{version}, err
	}
	if got := subnetAlerts(runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings()), subnetDepositHotkeyOwnerClass, ""); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	world = newSubnetWorld(t)
	original = world.contracts[world.coordinator].views["operatorAt"]
	world.contracts[world.coordinator].views["operatorAt"] = func(inputs []any) ([]any, error) {
		values, err := original(inputs)
		version := values[0].(stabi.STCoordinatorOperatorVersion)
		version.Active = false
		return []any{version}, err
	}
	hotkey := subnetDemandDepositHotkey(1)
	world.setStorage("SubtensorModule", "Owner", types.AccountID(subnetWorldKey("squatter")), hotkey[:])
	if got := subnetAlerts(runSubnetWorld(t, NewSubnetIdentitySignal(), world.signalSettings()), subnetDepositHotkeyOwnerClass, ""); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}
