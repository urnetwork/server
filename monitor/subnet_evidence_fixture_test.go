// A synthetic validator evidence journal and public history on top of the
// settlement simulator: one dual-key activation, write-once closed-census
// anchors with stored headers, anchored payload bytes, and the paged
// /sn/artifacts index, all served through the exact public routes.
package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
)

const subnetEvidenceTestStartBlock = 1000

// A synthetic stored commitment and its payload bytes.
type subnetEvidenceTestAnchor struct {
	stored  stabi.STValidatorEvidenceCommitment
	payload []byte
}

type subnetEvidenceTestJournal struct {
	t              *testing.T
	chain          *subnetSettlementTestChain
	address        common.Address
	deployed       uint64
	hotkey         [32]byte
	activation     [32]byte
	domainOverride *[32]byte
	anchors        map[[32]byte]*subnetEvidenceTestAnchor
	logs           []types.Log
	logIndex       uint
	payloads       map[string][]byte
	// Per-origin overrides of the public routes.
	missingArtifacts  map[int]map[[32]byte]bool
	tamperedArtifacts map[int]map[[32]byte]bool
	missingPayloads   map[int]map[[32]byte]bool
	tamperedPayloads  map[int]map[[32]byte]bool
	extraListed       map[int][]string
	disorderIndex     map[int]bool
}

// The synthetic validator hotkey; never a production identity.
var subnetEvidenceTestHotkey = [32]byte{0x7e, 0x57}

func newSubnetEvidenceTestJournal(t *testing.T, chain *subnetSettlementTestChain) *subnetEvidenceTestJournal {
	journal := &subnetEvidenceTestJournal{
		t: t, chain: chain, address: subnetReplayTestEvidence, deployed: subnetEvidenceTestStartBlock,
		hotkey: subnetEvidenceTestHotkey, activation: [32]byte{0xac, 0x71},
		anchors: map[[32]byte]*subnetEvidenceTestAnchor{}, payloads: map[string][]byte{},
		missingArtifacts: map[int]map[[32]byte]bool{0: {}, 1: {}}, tamperedArtifacts: map[int]map[[32]byte]bool{0: {}, 1: {}},
		missingPayloads: map[int]map[[32]byte]bool{0: {}, 1: {}}, tamperedPayloads: map[int]map[[32]byte]bool{0: {}, 1: {}},
		extraListed: map[int][]string{}, disorderIndex: map[int]bool{},
	}
	journal.emit("ActivationPublished", subnetEvidenceTestStartBlock+1, journal.activation, journal.hotkey, uint64(1), uint64(0))
	return journal
}

func (self *subnetEvidenceTestJournal) emit(name string, block uint64, values ...any) {
	self.logIndex += 1
	self.logs = append(self.logs, subnetReplayTestLog(self.t, self.chain.contracts.evidence, self.address, name, block, self.logIndex, subnetReplayTestTx(5000+uint64(self.logIndex)), values...))
}

// Anchors operator one's closed census for one epoch, computing the slot with
// the protocol's own header so the monitor's derivation is independently tested.
func (self *subnetEvidenceTestJournal) anchor(epoch uint64, block uint64) [32]byte {
	self.t.Helper()
	payload := []byte(`{"census":"synthetic","epoch":` + strconv.FormatUint(epoch, 10) + `}`)
	payloadHash := sha256.Sum256(payload)
	header := protocol.ValidatorEvidenceHeader{
		Domain: protocol.ValidatorEvidenceDomain{
			ChainID: 964, GenesisHash: subnetReplayTestGenesis, Netuid: 25,
			Coordinator: subnetReplayTestCoordinator, SettlementVault: subnetReplayTestVault,
			DeploymentIDHash: sha256.Sum256([]byte("synthetic-replay-deployment")), PolicyHash: subnetReplayTestPolicyHash,
			ActivationEpoch: 0, ActivationHash: self.activation,
		},
		Hotkey: self.hotkey, NoID: 1, Epoch: epoch, Kind: protocol.ValidatorEvidenceClosedCensus,
		VPK: [32]byte{0x9a}, BoundaryBlock: self.chain.end(epoch) - 1, BoundaryHash: [32]byte{0xb0},
		CensusHash: [32]byte{0xc5, byte(epoch)}, PayloadHash: payloadHash, PayloadBytes: uint64(len(payload)),
	}
	slot, err := header.SlotKey()
	if err != nil {
		self.t.Fatal(err)
	}
	digest, err := header.Digest()
	if err != nil {
		self.t.Fatal(err)
	}
	stored := stabi.ValidatorEvidenceHeaderFromProtocol(header)
	self.anchors[slot] = &subnetEvidenceTestAnchor{stored: stabi.STValidatorEvidenceCommitment{Header: stored, PublishedBlock: block}, payload: payload}
	self.payloads["0x"+hex.EncodeToString(payloadHash[:])] = payload
	self.emit("EvidenceCommitted", block, slot, uint64(1), epoch, digest, payloadHash, header.CensusHash, header.PayloadBytes)
	return slot
}

// A healthy journal: the activation and each closed epoch's census anchor.
func newHealthySubnetEvidenceJournal(t *testing.T, chain *subnetSettlementTestChain) *subnetEvidenceTestJournal {
	journal := newSubnetEvidenceTestJournal(t, chain)
	for epoch := uint64(0); epoch <= 3; epoch++ {
		journal.anchor(epoch, chain.end(epoch)+20)
	}
	return journal
}

func (self *subnetEvidenceTestJournal) handler(method string, args []any) ([]any, string) {
	switch method {
	case "coordinator":
		return []any{subnetReplayTestCoordinator}, ""
	case "settlementVault":
		return []any{subnetReplayTestVault}, ""
	case "chainId":
		return []any{uint64(964)}, ""
	case "netuid":
		return []any{uint16(25)}, ""
	case "genesisHash":
		return []any{[32]byte(subnetReplayTestGenesis)}, ""
	case "deploymentIdHash":
		if self.domainOverride != nil {
			return []any{*self.domainOverride}, ""
		}
		return []any{sha256.Sum256([]byte("synthetic-replay-deployment"))}, ""
	case "commitment":
		anchor, ok := self.anchors[args[0].([32]byte)]
		if !ok {
			return []any{stabi.STValidatorEvidenceCommitment{}}, ""
		}
		return []any{anchor.stored}, ""
	}
	self.t.Fatalf("unexpected evidence view %s", method)
	return nil, ""
}

// Installs the journal and the coordinator's link to it.
func (self *subnetEvidenceTestJournal) install(node *subnetReplayTestNode, fixed bool) {
	coordinator := node.contracts[subnetReplayTestCoordinator]
	inner := coordinator.handle
	coordinator.handle = func(method string, args []any) ([]any, string) {
		if method == "validatorEvidence" {
			if !fixed {
				return []any{common.Address{}}, ""
			}
			return []any{self.address}, ""
		}
		return inner(method, args)
	}
	node.contracts[self.address] = &subnetReplayTestContract{contract: self.chain.contracts.evidence, handle: self.handler, deployed: self.deployed}
	node.logs[self.address] = append([]types.Log(nil), self.logs...)
}

// The artifact history keys of every committed artifact.
func (self *subnetEvidenceTestJournal) historyKeys() []string {
	keys := []string{}
	for key, root := range self.chain.roots {
		keys = append(keys, "synthetic-prefix/st/v1/history/synthetic-replay-deployment/25/"+strconv.FormatUint(key.epoch, 10)+"/"+strconv.FormatUint(key.noId, 10)+"/"+hex.EncodeToString(root.ArtifactHash[:])+".json")
	}
	sort.Strings(keys)
	return keys
}

// Serves the three public routes per origin with the configured overrides.
func (self *subnetEvidenceTestJournal) apiHandler(t *testing.T, origins []string) func(*http.Request) (*http.Response, error) {
	artifacts := self.chain.apiHandler(t)
	return func(request *http.Request) (*http.Response, error) {
		origin := -1
		for i, candidate := range origins {
			if strings.HasPrefix(request.URL.String(), candidate+"/") {
				origin = i
			}
		}
		if origin < 0 || request.Header.Get("Authorization") != "" || request.Method != http.MethodGet {
			t.Fatal("request departed from the enrolled public origins")
		}
		query := request.URL.Query()
		switch request.URL.Path {
		case "/sn/artifacts":
			if query.Get("deployment_id") != "synthetic-replay-deployment" || query.Get("netuid") != "25" {
				t.Fatal("index request departed from the pinned deployment")
			}
			keys := append(self.historyKeys(), self.extraListed[origin]...)
			sort.Strings(keys)
			if self.disorderIndex[origin] && 1 < len(keys) {
				keys[0], keys[1] = keys[1], keys[0]
			}
			limit, _ := strconv.Atoi(query.Get("limit"))
			after := query.Get("after")
			selected := []map[string]any{}
			for _, key := range keys {
				if after != "" && key <= after && !self.disorderIndex[origin] {
					continue
				}
				if len(selected) == limit {
					break
				}
				hash := strings.TrimSuffix(key[strings.LastIndex(key, "/")+1:], ".json")
				selected = append(selected, map[string]any{"key": key, "size": 100, "content_hash": "sha256:" + hash})
			}
			more := len(selected) == limit && selected[len(selected)-1]["key"] != keys[len(keys)-1]
			nextAfter := ""
			if more {
				nextAfter = selected[len(selected)-1]["key"].(string)
			}
			body, _ := json.Marshal(map[string]any{"schema": "urnetwork-payout-artifact-history-v1", "objects": selected, "more": more, "next_after": nextAfter})
			return subnetReplayTestHttpResponse(request, http.StatusOK, body, ""), nil
		case "/sn/artifact":
			hash := strings.TrimPrefix(query.Get("hash"), "sha256:")
			decoded, _ := hex.DecodeString(hash)
			var key [32]byte
			copy(key[:], decoded)
			if self.missingArtifacts[origin][key] {
				return subnetReplayTestHttpResponse(request, http.StatusNotFound, []byte("missing"), ""), nil
			}
			response, err := artifacts(request)
			if err == nil && self.tamperedArtifacts[origin][key] && response.StatusCode == http.StatusOK {
				return subnetReplayTestHttpResponse(request, http.StatusOK, []byte(`{"schema":"tampered"}`), `"sha256:`+hash+`"`), nil
			}
			return response, err
		case "/sn/attempt-artifact":
			if query.Get("kind") != "metadata" || len(query) != 2 {
				t.Fatal("payload request departed from the typed metadata route")
			}
			hash := query.Get("hash")
			decoded, _ := hex.DecodeString(strings.TrimPrefix(hash, "0x"))
			var key [32]byte
			copy(key[:], decoded)
			payload, ok := self.payloads[hash]
			if !ok || self.missingPayloads[origin][key] {
				return subnetReplayTestHttpResponse(request, http.StatusNotFound, []byte("missing"), ""), nil
			}
			if self.tamperedPayloads[origin][key] {
				payload = append([]byte(nil), payload...)
				payload[0] ^= 0xff
			}
			return subnetReplayTestHttpResponse(request, http.StatusOK, payload, `"`+hash+`"`), nil
		}
		t.Fatalf("unexpected public route %s", request.URL.Path)
		return nil, nil
	}
}

// A ready evidence environment over the healthy settlement history.
func newSubnetEvidenceFixture(t *testing.T) (*subnetSettlementTestChain, *subnetEvidenceTestJournal, *subnetReplayTestNode, SignalSettings, *subnetReplayTestSource) {
	t.Helper()
	chain := newHealthySubnetSettlementChain(t)
	journal := newHealthySubnetEvidenceJournal(t, chain)
	node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, chain.finalizedBlock())
	chain.install(node)
	journal.install(node, true)
	settings, source := subnetReplayTestSettings(t, node)
	settings.SubnetReplay.EvidenceFromBlock = subnetEvidenceTestStartBlock
	settings.SubnetReplay.AnchorDeadlineBlocks = 30
	source.api = journal.apiHandler(t, settings.SubnetReplay.ApiOrigins)
	return chain, journal, node, settings, source
}

// Only the fixed semantic-replay capability gap is expected when healthy.
func requireSubnetEvidenceOnlySemanticGap(t *testing.T, alerts []Alert) {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == "subnet-monitor-coverage" && alert.Frame == "semantic-replay" {
			continue
		}
		t.Fatalf("healthy evidence alerted: %s %s %s %s", alert.Class, alert.Target, alert.Frame, alert.Observed)
	}
}
