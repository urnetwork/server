package controller

// sn_ctf_api_test.go — the capture the flag routes against sn/api/operator.yml:
// a view built from the document's own examples (epoch 9 drawn with its root
// pending, epoch 8 finalized) must answer exactly the example rows, and the
// null-draw rules, the top's ranking, the epoch and client answers and every
// route's conventions hold. Nothing here reads a database, Redis or a network.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"gopkg.in/yaml.v3"

	snapi "github.com/urfoundation/sn/api"
	"github.com/urfoundation/sn/ctf"

	"github.com/urnetwork/server"
)

// The examples of sn/api/operator.yml, by name.
func snCtfTestExamples(t testing.TB) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(snapi.OperatorYml, &doc); err != nil {
		t.Fatal(err)
	}
	examples := map[string]any{}
	for name, example := range doc["components"].(map[string]any)["examples"].(map[string]any) {
		examples[name] = example.(map[string]any)["value"]
	}
	return examples
}

// A value as JSON reads it back, so a Go answer and a YAML example compare.
func snCtfTestJson(t testing.TB, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func snCtfTestField(value any, path ...string) any {
	for _, key := range path {
		value = value.(map[string]any)[key]
	}
	return value
}

func requireSnCtfEqual(t testing.TB, what string, got any, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJson, _ := json.MarshalIndent(got, "", "  ")
		wantJson, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("%s:\n%s\nwant\n%s", what, gotJson, wantJson)
	}
}

func snCtfTestRao(rao uint64) uint256.Int {
	return *uint256.NewInt(rao)
}

func snCtfTestHex32(t testing.TB, text string) [32]byte {
	t.Helper()
	raw, err := hex.DecodeString(strings.TrimPrefix(text, "0x"))
	if err != nil || len(raw) != 32 {
		t.Fatalf("not 32 bytes: %s", text)
	}
	return [32]byte(raw)
}

func snCtfTestRef(t testing.TB, ref string) [32]byte {
	t.Helper()
	return snCtfTestHex32(t, strings.TrimPrefix(ref, "sha256:"))
}

func snCtfTestCountry(t testing.TB, code string) int {
	t.Helper()
	index, ok := snCtfCountryIndex()[code]
	if !ok {
		t.Fatalf("no country %s", code)
	}
	return index
}

// An epoch with 498 empty pots.
func snCtfTestEpoch(epoch uint64, startBlock uint64, root *snCtfRoot) *snCtfEpoch {
	covered := &snCtfEpoch{Epoch: epoch, StartBlock: startBlock, EndBlock: startBlock + 50_399, Root: root, Pots: make([]*snCtfPot, ctf.NumPots)}
	for i := range covered.Pots {
		covered.Pots[i] = &snCtfPot{}
	}
	return covered
}

func (self *snCtfEpoch) testPot(t testing.TB, code string, role int) *snCtfPot {
	t.Helper()
	return self.Pots[snCtfPotIndex(snCtfTestCountry(t, code), role)]
}

// The award rows of an example (CtfEpochResult.awards), as the view keeps them.
func snCtfTestExampleAwards(t testing.TB, rows any) []*snCtfAward {
	t.Helper()
	awards := []*snCtfAward{}
	for _, raw := range rows.([]any) {
		row := raw.(map[string]any)
		role := snCtfRoleProvider
		if row["role"] == "extender" {
			role = snCtfRoleExtender
		}
		clientId, err := server.ParseId(row["client_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		awards = append(awards, &snCtfAward{
			Role:       role,
			Country:    row["country"].(string),
			Rank:       row["rank"].(int),
			Unit:       snCtfTestHex32(t, row["unit_hash"].(string)),
			UnitFamily: row["unit_family"].(int),
			ClientId:   clientId,
			Coldkey:    snCtfTestHex32(t, row["coldkey"].(string)),
			Slot:       snCtfTestRao(uint64(row["slot"].(map[string]any)["rao"].(int))),
			Pot:        snCtfTestRao(uint64(row["pot_share"].(map[string]any)["rao"].(int))),
			Deferred:   row["pending"].(bool),
		})
	}
	return awards
}

// snCtfTestView is the documents' examples as a view: epoch 10 in progress,
// epoch 9 drawn under pending root 2, epoch 8 finalized under root 1, and the
// rows of Germany, Iceland and Turkmenistan as the examples give them. Every
// other country had no candidate and no winner.
func snCtfTestView(t testing.TB) *snCtfView {
	t.Helper()
	examples := snCtfTestExamples(t)
	state := examples["CtfState"]
	pool := snCtfTestField(state, "pool").(map[string]any)
	rootIndex, finalizedBlock := uint64(0), uint64(9_666_100)
	root8 := &snCtfRoot{Seq: 1, Finalized: true, RootIndex: &rootIndex, Epoch: 8, LedgerHash: [32]byte{0x08}, CommitBlock: 9_651_300, FinalizedBlock: &finalizedBlock,
		CommitTime: time.Date(2026, 12, 4, 22, 0, 0, 0, time.UTC), FinalizedTime: time.Date(2026, 12, 6, 22, 0, 0, 0, time.UTC)}
	root9 := &snCtfRoot{Seq: 2, Epoch: 9, LedgerHash: snCtfTestRef(t, snCtfTestField(state, "last_epoch", "root", "ledger_hash").(string)), CommitBlock: 9_701_994,
		CommitTime: time.Date(2026, 12, 11, 22, 24, 12, 0, time.UTC)}

	epoch8 := snCtfTestEpoch(8, 9_600_028, root8)
	epoch8.Drawn = true
	epoch8.Census = &snCtfCensusCommitment{Hash: [32]byte{0xc8}, Block: 9_650_612, Round: 34_535_980}
	epoch8.SlotBase = snCtfTestRao(48_025_660)
	*epoch8.testPot(t, "is", snCtfRoleProvider) = snCtfPot{Units: 2, Winners: 2, Out: snCtfTestRao(144_076_980), NearMisses: 0}
	*epoch8.testPot(t, "is", snCtfRoleExtender) = snCtfPot{Out: snCtfTestRao(240_128_300)}
	*epoch8.testPot(t, "de", snCtfRoleProvider) = snCtfPot{Units: 1_840, Winners: 5}
	*epoch8.testPot(t, "de", snCtfRoleExtender) = snCtfPot{Units: 2, Winners: 2, Out: snCtfTestRao(96_051_320)}
	clientId, err := server.ParseId("3f6c2a1e-8d4b-4c7a-9e21-5b0d7f3a9c64")
	if err != nil {
		t.Fatal(err)
	}
	coldkey := snCtfTestHex32(t, "0xff05d4de50cdc2b6d6aca7b376cfdb00f65c4243f6799c4eaca182e877faf619")
	epoch8.Awards = []*snCtfAward{
		{Role: snCtfRoleProvider, Country: "is", Rank: 1, Unit: [32]byte{0x81}, UnitFamily: 4, ClientId: server.NewId(), Coldkey: [32]byte{0x82}, Slot: snCtfTestRao(48_025_660)},
		{Role: snCtfRoleProvider, Country: "is", Rank: 2, Unit: [32]byte{0x83}, UnitFamily: 4, ClientId: clientId, Coldkey: coldkey, Slot: snCtfTestRao(48_025_660)},
	}

	epoch9 := snCtfTestEpoch(9, 9_650_428, root9)
	amounts := snCtfTestField(state, "last_epoch", "amounts").(map[string]any)
	rao := func(name string) uint256.Int {
		return snCtfTestRao(uint64(amounts[name].(map[string]any)["rao"].(int)))
	}
	epoch9.Drawn = true
	epoch9.Census = &snCtfCensusCommitment{Hash: [32]byte{0xc9}, Block: 9_701_016, Round: 34_737_596}
	epoch9.Funding, epoch9.Returned, epoch9.Owed, epoch9.Matured = rao("funding"), rao("returned"), rao("owed"), true
	epoch9.Fresh, epoch9.SlotBase, epoch9.Carried = rao("fresh"), rao("slot"), rao("carried")
	epoch9.PaidSlots, epoch9.PaidPots = snCtfTestRao(10*48_025_660), snCtfTestRao(2*28_815_396+3*19_210_264)
	*epoch9.testPot(t, "de", snCtfRoleProvider) = snCtfPot{Units: 1_862, Winners: 5, NearMisses: 7}
	*epoch9.testPot(t, "de", snCtfRoleExtender) = snCtfPot{Units: 3, Winners: 3, In: snCtfTestRao(96_051_320), Out: snCtfTestRao(134_471_848)}
	*epoch9.testPot(t, "is", snCtfRoleProvider) = snCtfPot{Units: 2, Winners: 2, In: snCtfTestRao(144_076_980), Out: snCtfTestRao(230_523_168), NearMisses: 1}
	*epoch9.testPot(t, "is", snCtfRoleExtender) = snCtfPot{In: snCtfTestRao(240_128_300), Out: snCtfTestRao(480_256_600)}
	*epoch9.testPot(t, "tm", snCtfRoleProvider) = snCtfPot{In: snCtfTestRao(240_128_300), Out: snCtfTestRao(480_256_600), NearMisses: 1}
	*epoch9.testPot(t, "tm", snCtfRoleExtender) = snCtfPot{In: snCtfTestRao(240_128_300), Out: snCtfTestRao(480_256_600)}
	epoch9.Awards = snCtfTestExampleAwards(t, snCtfTestField(examples["CtfEpochResult"], "awards"))
	for rank := 1; rank <= 5; rank += 1 {
		epoch9.Awards = append(epoch9.Awards, &snCtfAward{Role: snCtfRoleProvider, Country: "de", Rank: rank, Unit: [32]byte{0xd0, byte(rank)}, UnitFamily: 6, ClientId: server.NewId(), Coldkey: [32]byte{0xd1, byte(rank)}, Slot: snCtfTestRao(48_025_660)})
	}
	epoch9.AwardsRef, epoch9.PotsRef, epoch9.PendingRef = [32]byte{0xa9}, [32]byte{0xb9}, [32]byte{0xe9}

	return &snCtfView{
		Pool: snCtfPoolFacts{
			NoId:          uint64(pool["no_id"].(int)),
			PoolId:        uint64(pool["pool_id"].(int)),
			Vault:         common.HexToAddress(pool["vault"].(string)),
			CarryContract: common.HexToAddress(pool["carry_contract"].(string)),
			FirstEpoch:    uint64(pool["first_epoch"].(int)),
		},
		Clock:   snCtfClock{Epoch: 10, StartBlock: 9_700_828, EndBlock: 9_751_227, StartTime: time.Date(2026, 12, 11, 18, 30, 24, 0, time.UTC)},
		Roots:   []*snCtfRoot{root8, root9},
		Epochs:  []*snCtfEpoch{epoch8, epoch9},
		AsOf:    time.Date(2026, 12, 11, 22, 24, 12, 0, time.UTC),
		PageUrl: "https://ur.xyz/ctf",
	}
}

// The example's country rows, by code.
func snCtfTestExampleCountries(example any, path ...string) map[string]any {
	rows := map[string]any{}
	for _, row := range snCtfTestField(example, path...).([]any) {
		rows[row.(map[string]any)["code"].(string)] = row
	}
	return rows
}

// GET /sn/ctf answers the example's rows of Germany, Iceland and Turkmenistan
// exactly: pots, slots, units, near misses, what was awarded, the estimates and
// the chance to win, the flag and the epochs without a winner. The context is
// the example's too.
func TestSnCtfStateAnswersTheExample(t *testing.T) {
	view := snCtfTestView(t)
	example := snCtfTestJson(t, snCtfTestExamples(t)["CtfState"])
	got := snCtfTestJson(t, snCtfResultOf(view))
	exampleRows := snCtfTestExampleCountries(example, "countries")
	gotRows := snCtfTestExampleCountries(got, "countries")
	for _, code := range []string{"de", "is", "tm"} {
		requireSnCtfEqual(t, "country "+code, gotRows[code], exampleRows[code])
	}
	for _, field := range []string{"as_of", "unit", "current_epoch", "pending_draw", "definitions", "links"} {
		requireSnCtfEqual(t, field, snCtfTestField(got, field), snCtfTestField(example, field))
	}
	// the example's countries_version is illustrative; the pool is otherwise its
	pool := snCtfTestField(got, "pool").(map[string]any)
	requireSnCtfEqual(t, "countries_version", pool["countries_version"], "sha256:1dc01d610882808f4f3c56e8025d6fdac9d2b1cb8ca12c0458f4a367d2a17986")
	examplePool := snCtfTestField(example, "pool").(map[string]any)
	pool["countries_version"] = examplePool["countries_version"]
	requireSnCtfEqual(t, "pool", pool, examplePool)
	// last epoch's root and slots, and the amounts the epoch stored
	lastEpoch := snCtfTestField(got, "last_epoch").(map[string]any)
	exampleLastEpoch := snCtfTestField(example, "last_epoch").(map[string]any)
	requireSnCtfEqual(t, "root", lastEpoch["root"], exampleLastEpoch["root"])
	gotAmounts, exampleAmounts := lastEpoch["amounts"].(map[string]any), exampleLastEpoch["amounts"].(map[string]any)
	for _, name := range []string{"funding", "returned", "owed", "matured", "fresh", "slot", "pending_paid", "pending_in", "pending", "carried"} {
		requireSnCtfEqual(t, "amount "+name, gotAmounts[name], exampleAmounts[name])
	}
	// the fixture's awards: 10 slots, 2 fifths of Iceland's provider pot, 3 of
	// Germany's extender pot
	paid := uint64(10*48_025_660 + 2*28_815_396 + 3*19_210_264)
	requireSnCtfEqual(t, "awarded", gotAmounts["awarded"], snCtfTestJson(t, snCtfAmountOf(uint256.NewInt(paid))))
	requireSnCtfEqual(t, "paid", gotAmounts["paid"], snCtfTestJson(t, snCtfAmountOf(uint256.NewInt(paid))))
	requireSnCtfEqual(t, "slots", lastEpoch["slots"], snCtfTestJson(t, SnCtfSlots{Total: 2490, Won: 10, Open: 2480}))
	// the counts over all 249: only Germany's providers were fully captured
	requireSnCtfEqual(t, "totals", snCtfTestField(got, "totals"), snCtfTestJson(t, SnCtfTotals{Countries: 249, FlagsHeld: 0, NotFullyCaptured: &SnCtfNotFullyCaptured{Providers: 248, Extenders: 249}}))
	if countries := snCtfTestField(got, "countries").([]any); len(countries) != 249 || countries[0].(map[string]any)["code"] != "ad" || countries[248].(map[string]any)["code"] != "zw" {
		t.Fatalf("%d countries", len(countries))
	}
}

// A country held: all ten slots won.
func TestSnCtfFlagHeld(t *testing.T) {
	view := snCtfTestView(t)
	*view.lastEpoch().testPot(t, "de", snCtfRoleExtender) = snCtfPot{Units: 9, Winners: 5}
	result := snCtfResultOf(view)
	de := result.Countries[snCtfTestCountry(t, "de")]
	if !de.FlagHeld || result.Totals.FlagsHeld != 1 || result.Totals.NotFullyCaptured.Extenders != 248 {
		t.Fatalf("held %t, totals %+v", de.FlagHeld, result.Totals)
	}
	if *de.Extenders.ChanceToWin != 0.5 || *de.Extenders.Entries != 10 {
		t.Fatalf("chance %v of %d", *de.Extenders.ChanceToWin, *de.Extenders.Entries)
	}
}

// Before the first root: no last epoch, empty pots, no estimates and no draw
// figures, as the first-draw example has it.
func TestSnCtfStateBeforeTheFirstRoot(t *testing.T) {
	examples := snCtfTestExamples(t)
	example := snCtfTestJson(t, examples["CtfStateFirstDrawPending"])
	view := snCtfTestView(t)
	view.Roots, view.Epochs = nil, nil
	view.Clock = snCtfClock{Epoch: 9, StartBlock: 9_650_428, EndBlock: 9_700_827, StartTime: time.Date(2026, 12, 4, 18, 30, 24, 0, time.UTC)}
	view.Pending = &snCtfPendingDraw{Epoch: 8, Census: &snCtfCensusCommitment{
		Hash:  snCtfTestRef(t, snCtfTestField(example, "pending_draw", "census_hash").(string)),
		Block: 9_650_612, Round: 34_535_980,
	}}
	view.AsOf = time.Date(2026, 12, 4, 19, 7, 24, 0, time.UTC)
	got := snCtfTestJson(t, snCtfResultOf(view))
	requireSnCtfEqual(t, "iceland", snCtfTestExampleCountries(got, "countries")["is"], snCtfTestExampleCountries(example, "countries")["is"])
	for _, field := range []string{"as_of", "current_epoch", "last_epoch", "pending_draw", "totals"} {
		requireSnCtfEqual(t, field, snCtfTestField(got, field), snCtfTestField(example, field))
	}
	if top := snCtfTopOf(view, snCtfTopRoleAny, 10, false); len(top.Countries) != 0 || top.LastEpoch != nil {
		t.Fatalf("a top before the first root: %+v", top.Countries)
	}
	// a pending draw whose census is not committed yet names none of it
	view.Pending.Census = nil
	pending := snCtfResultOf(view).PendingDraw
	if pending.Epoch != 8 || pending.CensusHash != nil || pending.CensusBlock != nil || pending.Round != nil {
		t.Fatalf("pending %+v", pending)
	}
}

// A last epoch without a draw (no census committed in time, or one that is not
// canonical): the pots and the estimates stand, the draw figures are null, no
// flag is held, nothing is not fully captured, and the top is empty.
func TestSnCtfStateAfterAnEpochWithoutADraw(t *testing.T) {
	view := snCtfTestView(t)
	last := view.lastEpoch()
	last.Drawn = false
	result := snCtfResultOf(view)
	if result.LastEpoch == nil || result.LastEpoch.Drawn || result.Totals.NotFullyCaptured != nil {
		t.Fatalf("last epoch %+v, totals %+v", result.LastEpoch, result.Totals)
	}
	for _, country := range result.Countries {
		for _, role := range []SnCtfRoleState{country.Providers, country.Extenders} {
			if role.Slots != nil || role.Units != nil || role.NearMisses != nil || role.Entries != nil || role.ChanceToWin != nil || role.WinEstimate == nil {
				t.Fatalf("%s: %+v", country.Code, role)
			}
		}
		if country.NearMisses != nil || country.FlagHeld || country.Available == nil {
			t.Fatalf("%s: %+v", country.Code, country)
		}
	}
	is := result.Countries[snCtfTestCountry(t, "is")]
	if is.Providers.WinEstimate.Rao != 94_130_293 || is.Available.Rao != 1_191_036_368 {
		t.Fatalf("iceland %+v", is)
	}
	if top := snCtfTopOf(view, snCtfTopRoleProvider, 10, true); len(top.Countries) != 0 {
		t.Fatalf("a top without a draw: %d", len(top.Countries))
	}
	// the epoch's answer has its census and no draw
	detail := &snCtfEpochDetail{Draw: &snCtfDraw{Round: 1}}
	epoch := snCtfEpochResultOf(view, last, detail)
	if epoch.Census == nil || epoch.Draw != nil || epoch.Objects.Census == nil {
		t.Fatalf("census %+v draw %+v", epoch.Census, epoch.Draw)
	}
	for _, country := range epoch.Countries {
		if country.Providers.NearMisses != 0 || country.Extenders.NearMisses != 0 {
			t.Fatalf("%s near misses without a draw", country.Code)
		}
	}
	// an epoch with no census at all names none
	last.Census = nil
	if epoch := snCtfEpochResultOf(view, last, &snCtfEpochDetail{}); epoch.Census != nil || epoch.Objects.Census != nil || epoch.Draw != nil {
		t.Fatalf("census %+v", epoch.Census)
	}
}

// The top: the roles not fully captured at last epoch's draw, by pot, then
// code; both pots together for any; the seven uninhabited territories only when
// asked; and the limit.
func TestSnCtfTop(t *testing.T) {
	view := snCtfTestView(t)
	last := view.lastEpoch()
	*last.testPot(t, "aq", snCtfRoleProvider) = snCtfPot{Out: snCtfTestRao(9_000_000_000)}
	*last.testPot(t, "ad", snCtfRoleProvider) = snCtfPot{Out: snCtfTestRao(480_256_600)}
	codes := func(top *SnCtfTopResult) []string {
		out := []string{}
		for i, entry := range top.Countries {
			if entry.Rank != i+1 {
				t.Fatalf("rank %d at %d", entry.Rank, i)
			}
			out = append(out, fmt.Sprintf("%s:%d", entry.Country.Code, entry.OpenPot.Rao))
		}
		return out
	}
	requireSnCtfEqual(t, "providers", codes(snCtfTopOf(view, snCtfTopRoleProvider, 3, false)), []string{"ad:480256600", "tm:480256600", "is:230523168"})
	requireSnCtfEqual(t, "with the uninhabited", codes(snCtfTopOf(view, snCtfTopRoleProvider, 2, true)), []string{"aq:9000000000", "ad:480256600"})
	requireSnCtfEqual(t, "any", codes(snCtfTopOf(view, snCtfTopRoleAny, 3, false)), []string{"tm:960513200", "is:710779768", "ad:480256600"})
	requireSnCtfEqual(t, "extenders", codes(snCtfTopOf(view, snCtfTopRoleExtender, 3, false)), []string{"is:480256600", "tm:480256600", "de:134471848"})
	// Germany's providers were fully captured, so they never rank
	for _, entry := range snCtfTopOf(view, snCtfTopRoleProvider, 249, true).Countries {
		if entry.Country.Code == "de" {
			t.Fatal("a fully captured role ranked")
		}
	}
	top := snCtfTopOf(view, snCtfTopRoleProvider, 249, false)
	if len(top.Countries) != 248-7 || top.Role != snCtfTopRoleProvider || top.IncludeUninhabited {
		t.Fatalf("%d countries", len(top.Countries))
	}
}

// GET /sn/ctf/country/{cc}: the example's Iceland, its row and its history,
// newest first, with the medians and winning shares.
func TestSnCtfCountryAnswersTheExample(t *testing.T) {
	view := snCtfTestView(t)
	example := snCtfTestJson(t, snCtfTestExamples(t)["CtfCountryResult"])
	iceland, ok := snCtfParseCountry("IS")
	if !ok {
		t.Fatal("IS does not parse")
	}
	got := snCtfTestJson(t, snCtfCountryResultOf(view, iceland))
	for _, field := range []string{"as_of", "unit", "current_epoch", "pending_draw", "definitions", "links", "country", "history"} {
		requireSnCtfEqual(t, field, snCtfTestField(got, field), snCtfTestField(example, field))
	}
}

// GET /sn/ctf/epoch/{epoch}: the example's countries, awards and draw for
// epoch 9; the exclusions counted under the first rule each member failed in
// the document's order, not the census's.
func TestSnCtfEpochAnswersTheExample(t *testing.T) {
	view := snCtfTestView(t)
	example := snCtfTestJson(t, snCtfTestExamples(t)["CtfEpochResult"])
	exampleDraw := snCtfTestField(example, "draw").(map[string]any)
	signature, err := hex.DecodeString(strings.TrimPrefix(exampleDraw["signature"].(string), "0x"))
	if err != nil {
		t.Fatal(err)
	}
	detail := &snCtfEpochDetail{
		Candidates:     4_217,
		ExcusedSamples: 0,
		Draw:           &snCtfDraw{Round: 34_737_596, Signature: signature, Randomness: snCtfTestHex32(t, exampleDraw["randomness"].(string)), Seed: snCtfTestHex32(t, exampleDraw["seed"].(string))},
		Exclusions: []*snCtfExclusion{
			{Role: snCtfRoleProvider, Reason: snCtfReasonNoWallet, Count: 99},
			{Role: snCtfRoleProvider, Reason: snCtfReasonOfflineAtEnd, Count: 3},
			{Role: snCtfRoleProvider, Reason: snCtfReasonNoWallet, Count: 1},
			{Role: snCtfRoleExtender, Reason: snCtfReasonInactive, Count: 2},
			{Role: snCtfRoleExtender, Reason: snCtfReasonPresence, Count: 5},
		},
	}
	result := snCtfEpochResultOf(view, view.lastEpoch(), detail)
	got := snCtfTestJson(t, result)
	requireSnCtfEqual(t, "draw", snCtfTestField(got, "draw"), exampleDraw)
	gotCountries, exampleCountries := snCtfTestExampleCountries(got, "countries"), snCtfTestExampleCountries(example, "countries")
	for _, code := range []string{"de", "is"} {
		requireSnCtfEqual(t, "country "+code, gotCountries[code], exampleCountries[code])
	}
	// the example's awards, in its order, among the fixture's
	exampleAwards := snCtfTestField(example, "awards").([]any)
	gotAwards := []any{}
	for _, award := range snCtfTestField(got, "awards").([]any) {
		row := award.(map[string]any)
		if (row["role"] == "provider" && row["country"] == "is") || (row["role"] == "extender" && row["country"] == "de") {
			gotAwards = append(gotAwards, award)
		}
	}
	requireSnCtfEqual(t, "awards", gotAwards, exampleAwards)
	// providers first, then by country and rank
	order := []string{}
	for _, award := range result.Awards {
		order = append(order, fmt.Sprintf("%s/%s/%d", award.Role, award.Country, award.Rank))
	}
	requireSnCtfEqual(t, "award order", order, []string{"provider/de/1", "provider/de/2", "provider/de/3", "provider/de/4", "provider/de/5", "provider/is/1", "provider/is/2", "extender/de/1", "extender/de/2", "extender/de/3"})
	requireSnCtfEqual(t, "root", snCtfTestField(got, "root"), snCtfTestField(example, "root"))
	requireSnCtfEqual(t, "pending awards", snCtfTestField(got, "pending_awards"), snCtfTestField(example, "pending_awards"))
	if result.Census.Units != (SnCtfRoleCounts{Providers: 1_862 + 2, Extenders: 3}) || result.Census.Candidates != 4_217 || result.Census.Block != 9_701_016 {
		t.Fatalf("census %+v", result.Census)
	}
	// the census's published counts, summed over the countries, by name
	want := SnCtfExcluded{Providers: SnCtfExclusionCounts{NoWallet: 100, OfflineAtEnd: 3}, Extenders: SnCtfExclusionCounts{Inactive: 2, Presence: 5}}
	if result.Census.Excluded != want {
		t.Fatalf("excluded %+v", result.Census.Excluded)
	}
	if result.AsOf != "2026-12-11T22:24:12Z" || *result.Objects.Census != snCtfRef([32]byte{0xc9}) || result.Objects.Header != snCtfRef(view.lastEpoch().Root.LedgerHash) {
		t.Fatalf("as of %s objects %+v", result.AsOf, result.Objects)
	}
	// a finalized epoch's answer changes only with its root
	finalized := snCtfEpochResultOf(view, view.Epochs[0], &snCtfEpochDetail{})
	if finalized.AsOf != "2026-12-06T22:00:00Z" || finalized.Root.Status != "finalized" {
		t.Fatalf("epoch 8 as of %s, %s", finalized.AsOf, finalized.Root.Status)
	}
}

// GET /sn/ctf/client: the example's provider in Iceland, newest first, a
// candidate that won, a member left out for presence, and a role it was not in.
func TestSnCtfClientAnswersTheExample(t *testing.T) {
	view := snCtfTestView(t)
	example := snCtfTestJson(t, snCtfTestExamples(t)["CtfClientResult"])
	clientId, err := server.ParseId("3f6c2a1e-8d4b-4c7a-9e21-5b0d7f3a9c64")
	if err != nil {
		t.Fatal(err)
	}
	awardOf := func(epoch *snCtfEpoch) *snCtfAward {
		for _, award := range epoch.Awards {
			if award.ClientId == clientId {
				return award
			}
		}
		t.Fatalf("no award of the client in epoch %d", epoch.Epoch)
		return nil
	}
	facts := map[uint64]*snCtfClientEpochFacts{
		9: {
			RequiredSamples: 336,
			Members: map[int]*snCtfCensusMember{
				snCtfRoleProvider: {Role: snCtfRoleProvider, ClientId: clientId, Candidate: true, Country: "is", DrawCountry: "is", PresentSamples: 664, UnitFamily: 4},
				snCtfRoleExtender: {Role: snCtfRoleExtender, ClientId: clientId, Failed: 1 << snCtfReasonPresence, Country: "is", PresentSamples: 201, UnitFamily: 4},
			},
			Awards: map[int]*snCtfAward{snCtfRoleProvider: awardOf(view.Epochs[1])},
		},
		8: {
			RequiredSamples: 336,
			Members: map[int]*snCtfCensusMember{
				snCtfRoleProvider: {Role: snCtfRoleProvider, ClientId: clientId, Candidate: true, Country: "is", DrawCountry: "is", PresentSamples: 671, UnitFamily: 4},
			},
			Awards: map[int]*snCtfAward{snCtfRoleProvider: awardOf(view.Epochs[0])},
		},
	}
	// its coldkey's account is claimed through epoch 8's root, root 1
	coldkey := awardOf(view.Epochs[0]).Coldkey
	wins := []*snCtfWin{{Epoch: 9, Award: awardOf(view.Epochs[1])}, {Epoch: 8, Award: awardOf(view.Epochs[0])}}
	claims := map[[32]byte]*snCtfAccountClaims{coldkey: {Claimed: snCtfTestRao(48_025_660), Cumulative: map[uint64]uint256.Int{1: snCtfTestRao(48_025_660)}}}
	needs := snCtfClaimNeeds(view, wins)
	if len(needs) != 1 || needs[0].Coldkey != coldkey || len(needs[0].Roots) != 1 || needs[0].Roots[0] != view.Roots[0] {
		t.Fatalf("needs %+v: only epoch 8's root, finalized, is read", needs)
	}
	scope := snCtfClientScope{ClientId: &clientId, NetworkId: server.NewId()}
	got := snCtfTestJson(t, snCtfClientResultOf(view, scope, facts, wins, claims, 8))
	requireSnCtfEqual(t, "client", got, example)
	// limit 1: epoch 9 alone, and the claimed win of epoch 8 is left out
	if result := snCtfClientResultOf(view, scope, facts, wins, claims, 1); len(result.Epochs) != 1 || result.Epochs[0].Epoch != 9 || len(result.Wins) != 1 || result.Wins[0].Epoch != 9 {
		t.Fatalf("limit 1: %d epochs, %d wins", len(result.Epochs), len(result.Wins))
	}
	// a member left out by every rule but a candidate's own reads its first
	// rule in the document's order; a client never seen is not in the role
	member := &snCtfCensusMember{Role: snCtfRoleProvider, Failed: 1<<snCtfReasonHeadBound | 1<<snCtfReasonDuplicateAddress | 1<<snCtfReasonCountryChanged, Country: "zz", UnitFamily: 6}
	role := view.clientRole(view.Epochs[1], &snCtfClientEpochFacts{Members: map[int]*snCtfCensusMember{snCtfRoleProvider: member}}, snCtfRoleProvider)
	if *role.Reason != "country_changed" || role.Country != nil || *role.UnitFamily != 6 || role.UnitsInDraw != nil || role.Won {
		t.Fatalf("role %+v", role)
	}
	if empty := snCtfClientResultOf(&snCtfView{}, scope, nil, nil, nil, 8); len(empty.Epochs) != 0 || empty.Epochs == nil || len(empty.Wins) != 0 || empty.Wins == nil || *empty.ClientId != clientId.String() {
		t.Fatal("a client before the first root")
	}
	// a candidate of an epoch without a draw has no units in a draw
	view.Epochs[1].Drawn = false
	candidate := &snCtfCensusMember{Role: snCtfRoleProvider, Candidate: true, Country: "is", DrawCountry: "is", UnitFamily: 4}
	if role := view.clientRole(view.Epochs[1], &snCtfClientEpochFacts{Members: map[int]*snCtfCensusMember{snCtfRoleProvider: candidate}}, snCtfRoleProvider); role.UnitsInDraw != nil || !role.Candidate {
		t.Fatalf("role %+v", role)
	}
}

// GET /sn/ctf/client for a network's credential answers the document's
// network example: no client and no epochs, and the wins of its provider in
// Iceland and its extender in Germany, newest first, the Iceland provider's
// coldkey claimed through epoch 8's root and the extender's not at all.
func TestSnCtfClientAnswersTheNetworkExample(t *testing.T) {
	view := snCtfTestView(t)
	example := snCtfTestJson(t, snCtfTestExamples(t)["CtfClientResultNetwork"])
	iceland, err := server.ParseId("3f6c2a1e-8d4b-4c7a-9e21-5b0d7f3a9c64")
	if err != nil {
		t.Fatal(err)
	}
	extender, err := server.ParseId("6a0f3c9d-71e2-4b58-8c3a-e94d2b6f1057")
	if err != nil {
		t.Fatal(err)
	}
	extenderColdkey := snCtfTestHex32(t, "0xb515f4c4f7bc1e73557df4d34f7fb3ae7885d1fa1e522eea3439d1c198697afb")
	// the extender's epoch 8 win, beside the view's two winners in Iceland
	view.Epochs[0].Awards = append(view.Epochs[0].Awards, &snCtfAward{Role: snCtfRoleExtender, Country: "de", Rank: 1, Unit: [32]byte{0x84}, UnitFamily: 4,
		ClientId: extender, Coldkey: extenderColdkey, Slot: snCtfTestRao(48_025_660)})
	wins := []*snCtfWin{}
	for _, epoch := range view.Epochs {
		for _, award := range epoch.Awards {
			if award.ClientId == iceland || award.ClientId == extender {
				wins = append(wins, &snCtfWin{Epoch: epoch.Epoch, Award: award})
			}
		}
	}
	icelandColdkey := snCtfTestHex32(t, "0xff05d4de50cdc2b6d6aca7b376cfdb00f65c4243f6799c4eaca182e877faf619")
	claims := map[[32]byte]*snCtfAccountClaims{
		icelandColdkey:  {Claimed: snCtfTestRao(48_025_660), Cumulative: map[uint64]uint256.Int{1: snCtfTestRao(48_025_660)}},
		extenderColdkey: {Cumulative: map[uint64]uint256.Int{1: snCtfTestRao(48_025_660)}},
	}
	got := snCtfTestJson(t, snCtfClientResultOf(view, snCtfClientScope{NetworkId: server.NewId()}, nil, wins, claims, 8))
	requireSnCtfEqual(t, "network", got, example)
}

func TestSnCtfAmountsAndCountries(t *testing.T) {
	for rao, alpha := range map[uint64]string{0: "0.000000000", 1: "0.000000001", 48_025_660: "0.048025660", 1_000_000_000: "1.000000000", 212_273_417_481: "212.273417481"} {
		if amount := snCtfAmountOf(uint256.NewInt(rao)); amount.Alpha != alpha || amount.Rao != rao {
			t.Fatalf("%d: %+v", rao, amount)
		}
	}
	countries := snCtfCountries()
	if len(countries) != snCtfCountryCount || countries[0].Code != "ad" || countries[len(countries)-1].Code != "zw" {
		t.Fatalf("%d countries", len(countries))
	}
	if _, ok := snCtfParseCountry("xk"); ok {
		t.Fatal("Kosovo is in L")
	}
	uninhabited := []string{}
	for _, country := range countries {
		if !country.Inhabited {
			uninhabited = append(uninhabited, country.Code)
		}
	}
	requireSnCtfEqual(t, "uninhabited", uninhabited, []string{"aq", "bv", "gs", "hm", "io", "tf", "um"})
	for code, want := range map[string]string{
		"is": "Iceland", "us": "the United States", "gb": "the United Kingdom", "cd": "the DR Congo", "fk": "the Falkland Islands",
		"cc": "the Cocos (Keeling) Islands", "mf": "Saint Martin", "va": "Vatican City", "bq": "the Caribbean Netherlands", "ru": "Russia",
		"ps": "Palestine", "tw": "Taiwan", "ir": "Iran",
	} {
		if country, _ := snCtfParseCountry(code); country.SentenceName != want {
			t.Fatalf("%s: %q, want %q", code, country.SentenceName, want)
		}
	}
	if snCtfChanceToWin(5) != 1 || snCtfChanceToWin(1) != 1 || snCtfChanceToWin(6) != 5.0/6 || snCtfChanceToWin(100) != 0.05 {
		t.Fatal("chance to win")
	}
	if median := snCtfMedianAward([]*snCtfAward{{Slot: snCtfTestRao(5)}, {Slot: snCtfTestRao(1)}, {Slot: snCtfTestRao(3)}, {Slot: snCtfTestRao(9)}}); median.Rao != 3 {
		t.Fatalf("median %d", median.Rao)
	}
	if snCtfMedianAward(nil) != nil {
		t.Fatal("a median without winners")
	}
}

// snCtfTestSource answers from a fixed view.
type snCtfTestSource struct {
	view   *snCtfView
	err    error
	detail *snCtfEpochDetail
	// every scope's wins, and the coldkeys' claim states
	wins   []*snCtfWin
	claims map[[32]byte]*snCtfAccountClaims
}

func (self *snCtfTestSource) View(context.Context) (*snCtfView, error) {
	if self.err != nil {
		return nil, self.err
	}
	return self.view, nil
}

func (self *snCtfTestSource) EpochDetail(context.Context, *snCtfView, *snCtfEpoch) (*snCtfEpochDetail, error) {
	if self.detail == nil {
		return &snCtfEpochDetail{}, nil
	}
	return self.detail, nil
}

func (self *snCtfTestSource) ClientEpochs(context.Context, *snCtfView, server.Id, []*snCtfEpoch) (map[uint64]*snCtfClientEpochFacts, error) {
	return map[uint64]*snCtfClientEpochFacts{}, nil
}

func (self *snCtfTestSource) Wins(context.Context, *snCtfView, snCtfClientScope) ([]*snCtfWin, error) {
	return self.wins, nil
}

func (self *snCtfTestSource) AccountClaims(_ context.Context, _ *snCtfView, needs []*snCtfClaimNeed) map[[32]byte]*snCtfAccountClaims {
	claims := map[[32]byte]*snCtfAccountClaims{}
	for _, need := range needs {
		if claim, ok := self.claims[need.Coldkey]; ok {
			claims[need.Coldkey] = claim
		}
	}
	return claims
}

// withSnCtfTestSource serves the routes from source, behind a load balancer
// that answers for origins.
func withSnCtfTestSource(t testing.TB, source snCtfSource, origins ...string) {
	t.Helper()
	sourceOf, corsOrigins := snCtfSourceOf, snOperatorCorsOrigins
	snCtfSourceOf = func() (snCtfSource, error) {
		if source == nil {
			return nil, errSnCtfNotConfigured
		}
		return source, nil
	}
	snOperatorCorsOrigins = func() ([]string, bool) { return origins, true }
	snCtfResetViewCache()
	t.Cleanup(func() {
		snCtfSourceOf, snOperatorCorsOrigins = sourceOf, corsOrigins
		snCtfResetViewCache()
	})
}

func serveSnCtf(handler func(http.ResponseWriter, *http.Request), target string, headers ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Set(headers[i], headers[i+1])
	}
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}

func requireSnProblem(t testing.TB, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var problem SnProblem
	if response.Code != status || response.Header().Get("Content-Type") != "application/problem+json" || json.Unmarshal(response.Body.Bytes(), &problem) != nil ||
		problem != (SnProblem{Type: "about:blank", Title: http.StatusText(status), Status: status, Code: code, Detail: problem.Detail}) || problem.Detail == "" {
		t.Fatalf("%d %s %q, want a %d %s problem", response.Code, response.Header().Get("Content-Type"), response.Body.String(), status, code)
	}
	if values := response.Header().Values("Access-Control-Allow-Origin"); !slices.Equal(values, []string{"*"}) {
		t.Fatalf("Access-Control-Allow-Origin %v", values)
	}
}

// Every route but /sn/ctf/here is unavailable until capture the flag is
// configured, and while its state cannot be read: never an empty answer.
func TestSnCtfRoutesUnavailable(t *testing.T) {
	for _, source := range []snCtfSource{nil, &snCtfTestSource{err: errors.New("the database is down")}} {
		withSnCtfTestSource(t, source)
		requireSnProblem(t, serveSnCtf(SnCtf, "/sn/ctf"), http.StatusServiceUnavailable, SnProblemUnavailable)
		requireSnProblem(t, serveSnCtf(SnCtfTop, "/sn/ctf/top"), http.StatusServiceUnavailable, SnProblemUnavailable)
		requireSnProblem(t, serveSnCtf(func(w http.ResponseWriter, r *http.Request) { SnCtfCountry(w, r, "is") }, "/sn/ctf/country/is"), http.StatusServiceUnavailable, SnProblemUnavailable)
		requireSnProblem(t, serveSnCtf(func(w http.ResponseWriter, r *http.Request) { SnCtfEpoch(w, r, "9") }, "/sn/ctf/epoch/9"), http.StatusServiceUnavailable, SnProblemUnavailable)
	}
}

// The public answers: their Cache-Control, a strong ETag of the exact body with
// a 304 for it, Last-Modified from as_of, the Link, and one origin header.
func TestSnCtfRouteConventions(t *testing.T) {
	withSnCtfTestSource(t, &snCtfTestSource{view: snCtfTestView(t)})
	routes := []struct {
		target       string
		handler      func(http.ResponseWriter, *http.Request)
		cacheControl string
		lastModified string
	}{
		{"/sn/ctf", SnCtf, "public, max-age=60", "Fri, 11 Dec 2026 22:24:12 GMT"},
		{"/sn/ctf/top?role=provider&limit=2", SnCtfTop, "public, max-age=60", "Fri, 11 Dec 2026 22:24:12 GMT"},
		{"/sn/ctf/country/IS", func(w http.ResponseWriter, r *http.Request) { SnCtfCountry(w, r, "IS") }, "public, max-age=60", "Fri, 11 Dec 2026 22:24:12 GMT"},
		{"/sn/ctf/epoch/9", func(w http.ResponseWriter, r *http.Request) { SnCtfEpoch(w, r, "9") }, "public, max-age=60", "Fri, 11 Dec 2026 22:24:12 GMT"},
		{"/sn/ctf/epoch/8", func(w http.ResponseWriter, r *http.Request) { SnCtfEpoch(w, r, "8") }, "public, max-age=31536000, immutable", "Sun, 06 Dec 2026 22:00:00 GMT"},
	}
	for _, route := range routes {
		response := serveSnCtf(route.handler, route.target)
		header := response.Header()
		etag := header.Get("ETag")
		if response.Code != http.StatusOK || header.Get("Content-Type") != "application/json" || header.Get("Cache-Control") != route.cacheControl ||
			header.Get("Last-Modified") != route.lastModified || header.Get("Link") != SnOperatorLink || !strings.HasPrefix(etag, `"`) || !slices.Equal(header.Values("Access-Control-Allow-Origin"), []string{"*"}) {
			t.Fatalf("%s: %d %v", route.target, response.Code, header)
		}
		again := serveSnCtf(route.handler, route.target)
		if again.Header().Get("ETag") != etag || again.Body.String() != response.Body.String() {
			t.Fatalf("%s: the answer changed between two reads", route.target)
		}
		cached := serveSnCtf(route.handler, route.target, "If-None-Match", etag)
		if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 || cached.Header().Get("ETag") != etag || !slices.Equal(cached.Header().Values("Access-Control-Allow-Origin"), []string{"*"}) {
			t.Fatalf("%s: revalidation %d %v", route.target, cached.Code, cached.Header())
		}
	}
	var top SnCtfTopResult
	if response := serveSnCtf(SnCtfTop, "/sn/ctf/top?role=provider&limit=2"); json.Unmarshal(response.Body.Bytes(), &top) != nil || len(top.Countries) != 2 || top.Role != "provider" {
		t.Fatalf("top %+v", top)
	}
}

// Malformed parameters are 400s; a code outside the 249 and an epoch no root
// covers are 404s.
func TestSnCtfRouteErrors(t *testing.T) {
	withSnCtfTestSource(t, &snCtfTestSource{view: snCtfTestView(t)})
	country := func(code string) *httptest.ResponseRecorder {
		return serveSnCtf(func(w http.ResponseWriter, r *http.Request) { SnCtfCountry(w, r, code) }, "/sn/ctf/country/"+code)
	}
	epoch := func(value string) *httptest.ResponseRecorder {
		return serveSnCtf(func(w http.ResponseWriter, r *http.Request) { SnCtfEpoch(w, r, value) }, "/sn/ctf/epoch/"+value)
	}
	for _, code := range []string{"usa", "i", "1s", ""} {
		requireSnProblem(t, country(code), http.StatusBadRequest, SnProblemBadRequest)
	}
	requireSnProblem(t, country("xk"), http.StatusNotFound, SnProblemUnknownCountry)
	requireSnProblem(t, country("XK"), http.StatusNotFound, SnProblemUnknownCountry)
	for _, query := range []string{"role=all", "limit=0", "limit=250", "limit=x", "include_uninhabited=yes"} {
		requireSnProblem(t, serveSnCtf(SnCtfTop, "/sn/ctf/top?"+query), http.StatusBadRequest, SnProblemBadRequest)
	}
	for _, value := range []string{"x", "-1", "09", "99999999999999999999"} {
		requireSnProblem(t, epoch(value), http.StatusBadRequest, SnProblemBadRequest)
	}
	requireSnProblem(t, epoch("7"), http.StatusNotFound, SnProblemUnknownEpoch)
	requireSnProblem(t, epoch("10"), http.StatusNotFound, SnProblemUnknownEpoch)
	// no token, or one that does not authenticate (the database tests answer
	// the tokens that do: TestDbSnCtfClientAnswersEveryCredential)
	response := serveSnCtf(SnCtfClient, "/sn/ctf/client")
	requireSnProblem(t, response, http.StatusUnauthorized, SnProblemUnauthorized)
	if response.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate %q", response.Header().Get("WWW-Authenticate"))
	}
}

// One Access-Control-Allow-Origin: the load balancer answers for the origins on
// its allowlist, and for every origin when the list holds *; the api answers *
// for any other origin, or none.
func TestSnOperatorCors(t *testing.T) {
	withSnCtfTestSource(t, nil, "https://ur.xyz", "https://ur.io")
	for origin, want := range map[string][]string{"": {"*"}, "https://example.com": {"*"}, "https://ur.xyz": nil, "https://ur.io": nil, "https://UR.xyz": {"*"}} {
		response := serveSnCtf(SnOpenApi, "/sn/openapi.yml", "Origin", origin)
		if values := response.Header().Values("Access-Control-Allow-Origin"); !slices.Equal(values, want) {
			t.Fatalf("origin %q: %v, want %v", origin, values, want)
		}
	}
	snOperatorCorsOrigins = func() ([]string, bool) { return []string{"*"}, true }
	if values := serveSnCtf(SnOpenApi, "/sn/openapi.yml").Header().Values("Access-Control-Allow-Origin"); values != nil {
		t.Fatalf("with * on the list: %v", values)
	}
	// while the list is unknown, none: never a second value for the
	// operator's own sites
	snOperatorCorsOrigins = func() ([]string, bool) { return nil, false }
	for _, origin := range []string{"", "https://example.com", "https://ur.xyz"} {
		if values := serveSnCtf(SnOpenApi, "/sn/openapi.yml", "Origin", origin).Header().Values("Access-Control-Allow-Origin"); values != nil {
			t.Fatalf("origin %q with the list unknown: %v", origin, values)
		}
	}
	services := &snOperatorServicesYaml{}
	if err := yaml.Unmarshal([]byte("versions:\n  - services:\n      api:\n        cors_origins: [https://ur.xyz, https://ur.io]\n      mcp:\n        cors_origins_from: api\n  - services:\n      api:\n        cors_origins: [https://old.example]\n"), services); err != nil {
		t.Fatal(err)
	}
	requireSnCtfEqual(t, "api", snOperatorCorsOriginsOf(services, "api"), []string{"https://ur.xyz", "https://ur.io"})
	requireSnCtfEqual(t, "mcp", snOperatorCorsOriginsOf(services, "mcp"), []string{"https://ur.xyz", "https://ur.io"})
	if origins := snOperatorCorsOriginsOf(services, "web"); origins != nil {
		t.Fatalf("an unknown service: %v", origins)
	}
}

// The document, byte for byte, with its ETag and Cache-Control, and no Link to
// itself.
func TestSnOpenApi(t *testing.T) {
	withSnCtfTestSource(t, nil)
	response := serveSnCtf(SnOpenApi, "/sn/openapi.yml")
	header := response.Header()
	if response.Code != http.StatusOK || response.Body.String() != string(snapi.OperatorYml) || header.Get("Content-Type") != "application/yaml" ||
		header.Get("Cache-Control") != "public, max-age=3600" || header.Get("Link") != "" || header.Get("ETag") == "" {
		t.Fatalf("%d %v", response.Code, header)
	}
	cached := serveSnCtf(SnOpenApi, "/sn/openapi.yml", "If-None-Match", header.Get("ETag"))
	if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 {
		t.Fatalf("revalidation %d", cached.Code)
	}
}

// The caller's country, or null: never cached, with no ETag. The test hosts
// have no GeoLite2 record for a documentation address.
func TestSnCtfHere(t *testing.T) {
	withSnCtfTestSource(t, nil)
	response := serveSnCtf(SnCtfHere, "/sn/ctf/here")
	header := response.Header()
	var here SnCtfHereResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &here) != nil || header.Get("Cache-Control") != "private, no-store" ||
		header.Get("ETag") != "" || header.Get("Link") != SnOperatorLink || !slices.Equal(header.Values("Access-Control-Allow-Origin"), []string{"*"}) {
		t.Fatalf("%d %q %v", response.Code, response.Body.String(), header)
	}
	if here.Country != nil {
		if _, ok := snCtfParseCountry(*here.Country); !ok {
			t.Fatalf("country %q", *here.Country)
		}
	}
}
