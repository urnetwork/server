package controller

// The settlement amendments as the operator server reads them, and the SN25
// extender pool's configuration (sn WHITEPAPER.md §8.6.7). Pure: the published
// file is served here and signed with a generated key through the seams.

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/amendment"
	"github.com/urfoundation/sn/ledger"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type stAmendmentTestStore struct {
	raw []byte
}

func (self *stAmendmentTestStore) Load(ctx context.Context) ([]byte, error) { return self.raw, nil }
func (self *stAmendmentTestStore) Save(ctx context.Context, raw []byte) error {
	self.raw = raw
	return nil
}

// Serves a published file and pins key as the signer of chain 964 sn 25,
// through the seams, for the test's life.
type stAmendmentTestSite struct {
	key    *ecdsa.PrivateKey
	served atomic.Value
	alerts []string
	store  *stAmendmentTestStore
}

func newStAmendmentTestSite(t *testing.T) *stAmendmentTestSite {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	site := &stAmendmentTestSite{key: key, store: &stAmendmentTestStore{}}
	site.served.Store([]byte(nil))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := site.served.Load().([]byte)
		if raw == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(raw)
	}))
	previousSigner, previousUrl, previousStorer, previousSink := stAmendmentSigner, stAmendmentUrl, stAmendmentStorer, stLedgerV2AlertSink
	previousPlatformEndpoints := stPlatformEndpointsRead
	// the pure tests read an empty platform endpoint registry
	stPlatformEndpointsRead = func(context.Context) model.StPlatformEndpointSet { return model.StPlatformEndpointSet{} }
	stAmendmentSigner = func(chainId uint64, netuid uint16) (common.Address, bool) {
		return crypto.PubkeyToAddress(key.PublicKey), chainId == 964 && netuid == 25
	}
	stAmendmentUrl = server.URL + "/amendments.yml"
	stAmendmentStorer = func(cfg *StConfig) amendment.Store { return site.store }
	stLedgerV2AlertSink = func(name, subject, message string) { site.alerts = append(site.alerts, name+": "+message) }
	resetStAmendmentSources()
	t.Cleanup(func() {
		server.Close()
		stAmendmentSigner, stAmendmentUrl, stAmendmentStorer, stLedgerV2AlertSink = previousSigner, previousUrl, previousStorer, previousSink
		stPlatformEndpointsRead = previousPlatformEndpoints
		resetStAmendmentSources()
		stLedgerV2Alerted.Range(func(key, value any) bool {
			stLedgerV2Alerted.Delete(key)
			return true
		})
	})
	return site
}

func resetStAmendmentSources() {
	stDeploymentKeyAmendmentSourcesLock.Lock()
	defer stDeploymentKeyAmendmentSourcesLock.Unlock()
	stDeploymentKeyAmendmentSources = map[model.StDeploymentKey]*amendment.Source{}
}

// Serves a file of the signed extender entries for no 1 from each of
// epochs.
func (self *stAmendmentTestSite) publish(t *testing.T, epochs ...uint64) {
	t.Helper()
	var file strings.Builder
	file.WriteString("sn: 25\n")
	if len(epochs) > 0 {
		file.WriteString("signed_amendments:\n")
	}
	for _, epoch := range epochs {
		entry := amendment.NewExtenders(964, 25, 1, epoch)
		if err := entry.Sign(self.key); err != nil {
			t.Fatal(err)
		}
		line, err := entry.Line()
		if err != nil {
			t.Fatal(err)
		}
		file.WriteString("  - line: \"" + line + "\"\n    signature: \"0x" + hex.EncodeToString(entry.Signature) + "\"\n")
	}
	self.served.Store([]byte(file.String()))
}

// A mainnet operator's st.yml with the given blocks.
func stAmendmentTestConfig(t *testing.T, document string) *StConfig {
	t.Helper()
	var file stVaultFile
	if err := yaml.Unmarshal([]byte(document), &file); err != nil {
		t.Fatal(err)
	}
	cfg, err := stConfigForProfile("mainnet", file, []string{"http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.ChainId, cfg.Netuid, cfg.NoId = 964, 25, 1
	cfg.ContractAddress = common.HexToAddress("0x00000000000000000000000000000000000c0de1")
	return cfg
}

func stAmendmentTestLine(t *testing.T, epoch uint64) string {
	t.Helper()
	line, err := amendment.NewExtenders(964, 25, 1, epoch).Line()
	if err != nil {
		t.Fatal(err)
	}
	return line
}

// The signed entry fixes E_x: before it the amendment covers no epoch, from it
// every epoch, and the zero amendment is no amendment at all.
func TestStLedgerV2AmendmentsFromSignedEntry(t *testing.T) {
	site := newStAmendmentTestSite(t)
	site.publish(t, 8)
	cfg := stAmendmentTestConfig(t, "enabled: false\n")
	amendments, err := stLedgerV2ReadAmendments(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for epoch, want := range map[uint64]uint64{1: 0, 5: 0, 7: 0, 8: 8, 9: 8, 100: 8} {
		got, err := amendments.extenders(context.Background(), epoch)
		if err != nil || got.FirstEpoch != want || got.InForce(epoch) != (want != 0) {
			t.Fatalf("epoch %d: %+v, %v; want first epoch %d", epoch, got, err, want)
		}
	}
	if len(site.store.raw) == 0 {
		t.Fatal("the verified file is not stored")
	}
	// A later outage keeps the verified file, with an alert.
	site.served.Store([]byte(nil))
	if again, err := stLedgerV2ReadAmendments(context.Background(), cfg); err != nil {
		t.Fatalf("an outage stopped the reader: %v", err)
	} else if got, _ := again.extenders(context.Background(), 8); got.FirstEpoch != 8 {
		t.Fatalf("an outage dropped the entry: %+v", got)
	}
	if len(site.alerts) != 1 || !strings.Contains(site.alerts[0], stLedgerV2AlertAmendments) {
		t.Fatalf("alerts %v", site.alerts)
	}
}

// With no verified file held, what is in force is unknown: no epoch's rows
// are built, and configuration does not stand in.
func TestStLedgerV2AmendmentsUnknownWithoutAVerifiedFile(t *testing.T) {
	site := newStAmendmentTestSite(t)
	cfg := stAmendmentTestConfig(t, "enabled: false\namendments:\n  - \""+stAmendmentTestLine(t, 8)+"\"\n")
	if _, err := stLedgerV2ReadAmendments(context.Background(), cfg); !errors.Is(err, amendment.ErrUnknown) {
		t.Fatalf("%v, want ErrUnknown", err)
	}
	// a file signed by another key is no verified file
	other, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	site.key, other = other, site.key
	site.publish(t, 8)
	if _, err := stLedgerV2ReadAmendments(context.Background(), cfg); !errors.Is(err, amendment.ErrUnknown) {
		t.Fatalf("another signer's file: %v, want ErrUnknown", err)
	}
	if len(site.alerts) == 0 {
		t.Fatal("no alert")
	}
}

// Configuration is the fallback: a configured line covers the epochs before
// the first signed entry of its kind, and a signed entry, once in force,
// rules. A malformed or foreign configured line refuses the reader.
func TestStLedgerV2AmendmentsConfiguredFallback(t *testing.T) {
	site := newStAmendmentTestSite(t)
	site.publish(t, 9)
	cfg := stAmendmentTestConfig(t, "enabled: false\namendments:\n  - \""+stAmendmentTestLine(t, 8)+"\"\n")
	amendments, err := stLedgerV2ReadAmendments(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for epoch, want := range map[uint64]uint64{7: 0, 8: 8, 9: 9, 10: 9} {
		if got, err := amendments.extenders(context.Background(), epoch); err != nil || got.FirstEpoch != want {
			t.Fatalf("epoch %d: %+v, %v; want first epoch %d", epoch, got, err, want)
		}
	}
	for name, document := range map[string]string{
		"malformed":                "enabled: false\namendments:\n  - \"" + strings.Replace(stAmendmentTestLine(t, 8), "zeta 1", "zeta 2", 1) + "\"\n",
		"another deployment":       "enabled: false\namendments:\n  - \"" + strings.Replace(stAmendmentTestLine(t, 8), "chain 964", "chain 945", 1) + "\"\n",
		"not a list of lines":      "enabled: false\namendments:\n  line: x\n",
		"two lines from one epoch": "enabled: false\namendments:\n  - \"" + stAmendmentTestLine(t, 8) + "\"\n  - \"" + stAmendmentTestLine(t, 8) + "\"\n",
	} {
		if _, err := stLedgerV2ReadAmendments(context.Background(), stAmendmentTestConfig(t, document)); err == nil {
			t.Fatalf("%s: the configured lines were accepted", name)
		}
	}
}

// A deployment without a pinned signer publishes no amendments: only its
// configured lines can put one in force, and nothing is fetched.
func TestStLedgerV2AmendmentsUnpinnedDeployment(t *testing.T) {
	newStAmendmentTestSite(t)
	line, err := amendment.NewExtenders(945, 350, 1, 8).Line()
	if err != nil {
		t.Fatal(err)
	}
	cfg := stAmendmentTestConfig(t, "enabled: false\namendments:\n  - \""+line+"\"\n")
	cfg.ChainId, cfg.Netuid = 945, 350
	amendments, err := stLedgerV2ReadAmendments(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := amendments.extenders(context.Background(), 8); got.FirstEpoch != 8 {
		t.Fatalf("epoch 8: %+v", got)
	}
	cfg = stAmendmentTestConfig(t, "enabled: false\n")
	cfg.ChainId, cfg.Netuid = 945, 350
	if amendments, err = stLedgerV2ReadAmendments(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got, _ := amendments.extenders(context.Background(), 1000); got.InForce(1000) {
		t.Fatalf("an unconfigured deployment has an amendment: %+v", got)
	}
}

// Testing_SetStAmendments unpins every deployment for another package's test,
// such as sn's end-to-end chain, which borrows mainnet's identity: nothing is
// fetched and no amendment is in force, and the restore pins mainnet again.
func TestStLedgerV2AmendmentsTestingHook(t *testing.T) {
	site := newStAmendmentTestSite(t)
	site.publish(t, 8)
	cfg := stAmendmentTestConfig(t, "enabled: false\n")
	pinnedUrl, pinnedSigner := stAmendmentUrl, stAmendmentSigner
	restore := Testing_SetStAmendments("", common.Address{}, 0, 0)
	amendments, err := stLedgerV2ReadAmendments(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := amendments.extenders(context.Background(), 8); got.InForce(8) {
		t.Fatalf("an unpinned deployment read the published entry: %+v", got)
	}
	restore()
	if stAmendmentUrl != pinnedUrl {
		t.Fatal("the restore did not restore the published address")
	}
	if _, pinned := stAmendmentSigner(964, 25); !pinned {
		t.Fatal("the restore did not pin the deployment again")
	}
	stAmendmentSigner = pinnedSigner
	if amendments, err = stLedgerV2ReadAmendments(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got, _ := amendments.extenders(context.Background(), 8); got.FirstEpoch != 8 {
		t.Fatalf("after the restore: %+v", got)
	}
}

// st.yml's extender_pool block: the measurement networks Z leaves out, and
// the networks the operator declares it controls (E5). An entry that is not a
// network id refuses the block rather than counting what it meant to leave
// out. The testnet profile reads its own blocks.
func TestStExtenderPoolSettings(t *testing.T) {
	measurement, declared := server.NewId(), server.NewId()
	cfg := stAmendmentTestConfig(t, "enabled: false\nextender_pool:\n  measurement_network_ids:\n    - "+measurement.String()+"\n  declared_network_ids:\n    - \" "+declared.String()+" \"\n")
	pool, err := stExtenderPoolSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pool.measurementNetworkIds) != 1 || !pool.measurementNetworkIds[measurement] || len(pool.declaredNetworkIds) != 1 || !pool.declaredNetworkIds[declared] {
		t.Fatalf("pool %+v", pool)
	}
	if pool, err := stExtenderPoolSettings(stAmendmentTestConfig(t, "enabled: false\n")); err != nil || len(pool.measurementNetworkIds) != 0 || len(pool.declaredNetworkIds) != 0 {
		t.Fatalf("no block: %+v, %v", pool, err)
	}
	for _, document := range []string{
		"enabled: false\nextender_pool:\n  measurement_network_ids:\n    - not-an-id\n",
		"enabled: false\nextender_pool:\n  declared_network_ids:\n    - \"\"\n",
		"enabled: false\nextender_pool:\n  measurement_network_ids: 7\n",
	} {
		if _, err := stExtenderPoolSettings(stAmendmentTestConfig(t, document)); err == nil {
			t.Fatalf("accepted %q", document)
		}
	}
	var file stVaultFile
	if err := yaml.Unmarshal([]byte("testnet-extender-pool:\n  declared_network_ids:\n    - "+declared.String()+"\nextender_pool:\n  declared_network_ids:\n    - "+measurement.String()+"\n"), &file); err != nil {
		t.Fatal(err)
	}
	testnet, err := stConfigForProfile("testnet", file, []string{"http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if pool, err := stExtenderPoolSettings(testnet); err != nil || !pool.declaredNetworkIds[declared] || pool.declaredNetworkIds[measurement] {
		t.Fatalf("testnet reads %+v, %v", pool, err)
	}
}

// A recorded epoch's extender component must match the amendment: rows
// recorded without one for an epoch it covers, or with one for an epoch it
// does not, or one on an unrecoverable epoch, are refused before anything is
// read; the rest carry nothing.
func TestStLedgerV2RecordedExtendersMatchTheAmendment(t *testing.T) {
	amendment := model.StExtenderAmendment{FirstEpoch: 8}
	component := &model.StLedgerV2EpochExtenders{Bytes: 5, CensusChunks: []string{}, IntraNetwork: 5, Contracts: 1, Payers: 1}
	for _, c := range []struct {
		name    string
		epoch   uint64
		rows    *model.StLedgerV2EpochRows
		refused bool
	}{
		{name: "before E_x without a component", epoch: 7, rows: &model.StLedgerV2EpochRows{ProviderChunks: []string{}}},
		{name: "before E_x with one", epoch: 7, rows: &model.StLedgerV2EpochRows{ProviderChunks: []string{}, Extenders: component}, refused: true},
		{name: "from E_x without one", epoch: 8, rows: &model.StLedgerV2EpochRows{ProviderChunks: []string{}}, refused: true},
		{name: "unrecoverable from E_x", epoch: 8, rows: &model.StLedgerV2EpochRows{DeclaredBy: "operator"}},
		{name: "unrecoverable with one", epoch: 8, rows: &model.StLedgerV2EpochRows{DeclaredBy: "operator", Extenders: component}, refused: true},
	} {
		c.rows.Epoch = c.epoch
		input, err := stLedgerV2RecordedExtenders(context.Background(), &StLedgerV2CoveredEpoch{Epoch: c.epoch, Rows: c.rows}, amendment)
		if (err != nil) != c.refused || input != nil {
			t.Fatalf("%s: %+v, %v", c.name, input, err)
		}
	}
}

// A built epoch's component is recorded as its Z and its census chunks, with
// the parts of the pool it was built from: Z's two parts and the contracts and
// payers behind them. A component without parts, or with parts of another Z,
// is refused, and so is a component's parts without a component.
func TestStLedgerV2EpochExtendersRecord(t *testing.T) {
	if got, err := stLedgerV2EpochExtenders(&ledger.EpochAllocation{}, nil); got != nil || err != nil {
		t.Fatalf("an epoch without a component records %+v, %v", got, err)
	}
	census := []ledger.Ref{ledger.RefOf([]byte("a")), ledger.RefOf([]byte("b"))}
	allocation := &ledger.EpochAllocation{Epoch: 8, Extenders: &ledger.ExtenderAllocation{ExtenderSplit: ledger.ExtenderSplit{Bytes: 42}, Census: census}}
	pool := &model.StEpochExtenderPool{IntraPlatform: 2, IntraNetwork: 40, Contracts: 3, Payers: 2}
	got, err := stLedgerV2EpochExtenders(allocation, stLedgerV2ExtenderParts(pool))
	if err != nil {
		t.Fatal(err)
	}
	want := &model.StLedgerV2EpochExtenders{Bytes: 42, CensusChunks: []string{census[0].String(), census[1].String()}, IntraPlatform: 2, IntraNetwork: 40, Contracts: 3, Payers: 2}
	if !got.Equal(want) {
		t.Fatalf("recorded %+v, want %+v", got, want)
	}
	if got.Equal(nil) || (*model.StLedgerV2EpochExtenders)(nil).Equal(want) || !(*model.StLedgerV2EpochExtenders)(nil).Equal(nil) {
		t.Fatal("Equal does not tell a component from none")
	}
	for name, other := range map[string]*model.StLedgerV2EpochExtenders{
		"another census":         {Bytes: 42, CensusChunks: []string{census[0].String()}, IntraPlatform: 2, IntraNetwork: 40, Contracts: 3, Payers: 2},
		"another split of Z":     {Bytes: 42, CensusChunks: want.CensusChunks, IntraPlatform: 1, IntraNetwork: 41, Contracts: 3, Payers: 2},
		"another contract count": {Bytes: 42, CensusChunks: want.CensusChunks, IntraPlatform: 2, IntraNetwork: 40, Contracts: 4, Payers: 2},
		"another payer count":    {Bytes: 42, CensusChunks: want.CensusChunks, IntraPlatform: 2, IntraNetwork: 40, Contracts: 3, Payers: 3},
	} {
		if got.Equal(other) {
			t.Fatalf("%s: Equal holds", name)
		}
	}
	for name, parts := range map[string]*model.StLedgerV2EpochExtenders{
		"no parts":          nil,
		"another Z's parts": stLedgerV2ExtenderParts(&model.StEpochExtenderPool{IntraNetwork: 41, Contracts: 1, Payers: 1}),
	} {
		if got, err := stLedgerV2EpochExtenders(allocation, parts); err == nil {
			t.Fatalf("%s: recorded %+v", name, got)
		}
	}
	if got, err := stLedgerV2EpochExtenders(&ledger.EpochAllocation{}, stLedgerV2ExtenderParts(pool)); err == nil {
		t.Fatalf("parts without a component recorded %+v", got)
	}
}

// What the epoch reader's pool holds by construction, a record holds too: Z is
// its parts' sum, and the contracts and payers behind Z exist exactly when Z is
// positive, with no more payers than contracts.
func TestStLedgerV2EpochExtendersCheck(t *testing.T) {
	for name, c := range map[string]struct {
		component model.StLedgerV2EpochExtenders
		valid     bool
	}{
		"Z of both parts":     {model.StLedgerV2EpochExtenders{Bytes: 3, IntraPlatform: 1, IntraNetwork: 2, Contracts: 2, Payers: 1}, true},
		"no Z":                {model.StLedgerV2EpochExtenders{}, true},
		"Z not its parts":     {model.StLedgerV2EpochExtenders{Bytes: 4, IntraPlatform: 1, IntraNetwork: 2, Contracts: 2, Payers: 1}, false},
		"parts that overflow": {model.StLedgerV2EpochExtenders{Bytes: 0, IntraPlatform: math.MaxUint64, IntraNetwork: 1, Contracts: 1, Payers: 1}, false},
		"Z without contracts": {model.StLedgerV2EpochExtenders{Bytes: 3, IntraNetwork: 3}, false},
		"contracts without Z": {model.StLedgerV2EpochExtenders{Contracts: 1, Payers: 1}, false},
		"more payers":         {model.StLedgerV2EpochExtenders{Bytes: 3, IntraNetwork: 3, Contracts: 1, Payers: 2}, false},
		"contracts, no payer": {model.StLedgerV2EpochExtenders{Bytes: 3, IntraNetwork: 3, Contracts: 1}, false},
		"more than a record":  {model.StLedgerV2EpochExtenders{Bytes: 3, IntraNetwork: 3, Contracts: math.MaxInt64 + 1, Payers: 1}, false},
	} {
		if err := c.component.Check(); (err == nil) != c.valid {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
