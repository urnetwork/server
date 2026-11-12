package controller

// sn_ctf_api_state.go — the answers of the capture the flag routes, derived from
// what the CTF pool's committed roots fixed (sn CAPTURETHEFLAG.md §4.5, §5,
// §9.1; sn/api/operator.yml). Pure: the facts come in a snCtfView, read by
// sn_ctf_api_source.go, so every rule here is tested without a database.
//
// Vocabulary (operator.yml):
//   - last epoch is the latest epoch a committed CTF root covers; its draw is
//     the basis of the draw figures, which are null without one;
//   - a role's pot is the pot carried out of last epoch into the next draw;
//   - a role is not fully captured where one of its five slots went unclaimed.

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server"
)

// The roles, as the census, award and pot rows number them.
const (
	snCtfRoleProvider = 1
	snCtfRoleExtender = 2
)

// Slots per country and role, and in all.
const (
	snCtfRanks = 5
	snCtfSlots = 10 * snCtfCountryCount
)

// The samples of an epoch's presence rule, one each quarter hour.
const snCtfSamplesPerEpoch = 672

// Seconds per block of the estimates of times of blocks to come.
const snCtfBlockSeconds = 12

func snCtfRoleName(role int) string {
	if role == snCtfRoleExtender {
		return "extender"
	}
	return "provider"
}

// snCtfPotIndex is pot (country, role)'s position: 2·c + role − 1.
func snCtfPotIndex(country int, role int) int {
	return 2*country + role - 1
}

// snCtfPoolFacts is the CTF pool's identity on chain.
type snCtfPoolFacts struct {
	NoId          uint64
	PoolId        uint64
	Vault         common.Address
	CarryContract common.Address
	FirstEpoch    uint64
}

// snCtfClock is this epoch.
type snCtfClock struct {
	Epoch      uint64
	StartBlock uint64
	EndBlock   uint64
	// the start block's header time
	StartTime time.Time
}

// snCtfRoot is a committed CTF root: pending, or finalized. A replaced or
// deleted root covers nothing.
type snCtfRoot struct {
	Seq            uint64
	Finalized      bool
	RootIndex      *uint64
	Epoch          uint64
	LedgerHash     [32]byte
	CommitBlock    uint64
	FinalizedBlock *uint64
	CommitTime     time.Time
	// zero while pending
	FinalizedTime time.Time
}

func (self *snCtfRoot) status() string {
	if self.Finalized {
		return "finalized"
	}
	return "pending"
}

// The last time anything about the root changed: its finalization, else its
// commitment.
func (self *snCtfRoot) changeTime() time.Time {
	if self.Finalized && !self.FinalizedTime.IsZero() {
		return self.FinalizedTime
	}
	return self.CommitTime
}

// snCtfPot is one (country, role) of an epoch: its draw and its pot.
type snCtfPot struct {
	Units   uint32
	Winners int
	In      uint256.Int
	Out     uint256.Int
	// the miners the census left out only for presence or a change of country
	NearMisses int64
}

// snCtfAward is one winner, as its award row records it.
type snCtfAward struct {
	Role       int
	Country    string
	Rank       int
	Unit       [32]byte
	UnitFamily int
	ClientId   server.Id
	Coldkey    [32]byte
	Slot       uint256.Int
	Pot        uint256.Int
	Deferred   bool
}

func (self *snCtfAward) total() *uint256.Int {
	return new(uint256.Int).Add(&self.Slot, &self.Pot)
}

// snCtfPendingAward is a pot share still owed after an epoch.
type snCtfPendingAward struct {
	AwardEpoch uint64
	Role       int
	Country    string
	Rank       int
	Coldkey    [32]byte
	Amount     uint256.Int
}

// snCtfCensusCommitment is an epoch's census as committed on chain.
type snCtfCensusCommitment struct {
	Hash  [32]byte
	Block uint64
	// ts(B_k), the commitment block's time
	Time time.Time
	// ρ_k, the drand round that draws it
	Round uint64
}

// snCtfEpoch is one covered epoch as its root fixed it (ctf.EpochResult and
// its award and pot rows).
type snCtfEpoch struct {
	Epoch      uint64
	StartBlock uint64
	EndBlock   uint64
	// the root that covers it
	Root *snCtfRoot
	// a census was committed in time and the draw ran
	Drawn bool
	// nil when no census was committed in time
	Census *snCtfCensusCommitment
	// f_k, R_k, X_k, M_k, F_k and the slot amount floor(F_k / 2490)
	Funding  uint256.Int
	Returned uint256.Int
	Owed     uint256.Int
	Matured  bool
	Fresh    uint256.Int
	SlotBase uint256.Int
	// what the root paid for the epoch, and the pending awards outstanding after it
	PaidSlots    uint256.Int
	PaidPots     uint256.Int
	PaidPending  uint256.Int
	PendingTotal uint256.Int
	// Σ pot out
	Carried uint256.Int
	// the 498 pots, in pot order
	Pots   []*snCtfPot
	Awards []*snCtfAward
	// the ledger objects of the epoch
	AwardsRef  [32]byte
	PotsRef    [32]byte
	PendingRef [32]byte
}

func (self *snCtfEpoch) pot(country int, role int) *snCtfPot {
	if index := snCtfPotIndex(country, role); index < len(self.Pots) && self.Pots[index] != nil {
		return self.Pots[index]
	}
	return &snCtfPot{}
}

func (self *snCtfEpoch) won() int {
	won := 0
	for _, pot := range self.Pots {
		if pot != nil {
			won += pot.Winners
		}
	}
	return won
}

// What the winners of one (country, role) won, slots and pot shares.
func (self *snCtfEpoch) awarded(country string, role int) *uint256.Int {
	awarded := new(uint256.Int)
	for _, award := range self.Awards {
		if award.Country == country && award.Role == role {
			awarded.Add(awarded, award.total())
		}
	}
	return awarded
}

// The awards of one (country, role), by rank.
func (self *snCtfEpoch) awardsOf(country string, role int) []*snCtfAward {
	awards := []*snCtfAward{}
	for _, award := range self.Awards {
		if award.Country == country && award.Role == role {
			awards = append(awards, award)
		}
	}
	slices.SortFunc(awards, func(a, b *snCtfAward) int { return cmp.Compare(a.Rank, b.Rank) })
	return awards
}

// snCtfPendingDraw is an ended epoch that no committed root covers yet.
type snCtfPendingDraw struct {
	Epoch uint64
	// nil until the census is committed
	Census *snCtfCensusCommitment
}

// snCtfView is what the routes answer from, read at one time: the pool, this
// epoch, the committed roots, every covered epoch from the pool's first, and
// the pending draw. AsOf is when any of it last changed.
type snCtfView struct {
	Pool    snCtfPoolFacts
	Clock   snCtfClock
	Roots   []*snCtfRoot
	Epochs  []*snCtfEpoch
	Pending *snCtfPendingDraw
	AsOf    time.Time
	// links.page of the hub; a country's page is PageUrl + "/" + code
	PageUrl string
}

// The latest covered epoch: last epoch, nil before the pool's first root.
func (self *snCtfView) lastEpoch() *snCtfEpoch {
	if len(self.Epochs) == 0 {
		return nil
	}
	return self.Epochs[len(self.Epochs)-1]
}

func (self *snCtfView) epoch(epoch uint64) *snCtfEpoch {
	for _, covered := range self.Epochs {
		if covered.Epoch == epoch {
			return covered
		}
	}
	return nil
}

// The pending awards owed before an epoch: those outstanding after the covered
// epoch before it.
func (self *snCtfView) pendingIn(epoch *snCtfEpoch) uint256.Int {
	var before *snCtfEpoch
	for _, covered := range self.Epochs {
		if covered.Epoch < epoch.Epoch {
			before = covered
		}
	}
	if before == nil {
		return uint256.Int{}
	}
	return before.PendingTotal
}

// Amounts

// snCtfAmountOf is an exact amount: alpha with exactly nine fractional digits,
// and rao. CTF amounts stay far below 2^53 rao; a rao beyond 2^64 saturates
// while alpha stays exact.
func snCtfAmountOf(rao *uint256.Int) SnCtfAmount {
	digits := rao.Dec()
	if len(digits) < 10 {
		digits = strings.Repeat("0", 10-len(digits)) + digits
	}
	amount := SnCtfAmount{Alpha: digits[:len(digits)-9] + "." + digits[len(digits)-9:], Rao: math.MaxUint64}
	if rao.IsUint64() {
		amount.Rao = rao.Uint64()
	}
	return amount
}

func snCtfAmountPointer(rao *uint256.Int) *SnCtfAmount {
	amount := snCtfAmountOf(rao)
	return &amount
}

func snCtfSum(amounts ...*uint256.Int) *uint256.Int {
	sum := new(uint256.Int)
	for _, amount := range amounts {
		sum.Add(sum, amount)
	}
	return sum
}

func snCtfUnit() SnCtfUnit {
	return SnCtfUnit{Token: "alpha", Symbol: "α", Netuid: 25, Decimals: 9}
}

func snCtfRef(hash [32]byte) string {
	return "sha256:" + fmt.Sprintf("%x", hash)
}

func snCtfHex(bytes []byte) string {
	return fmt.Sprintf("0x%x", bytes)
}

func snCtfAddress(address common.Address) string {
	return strings.ToLower(address.Hex())
}

func snCtfColdkeySs58(coldkey [32]byte) string {
	address, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
	if err != nil {
		return ""
	}
	return address
}

// snCtfCarryColdkey is the carry contract's coldkey: the EVM mirror of its
// address, blake2b-256("evm:" ‖ address).
func snCtfCarryColdkey(carryContract common.Address) string {
	coldkey := ss58.EvmMirrorPubkey([20]byte(carryContract))
	return snCtfHex(coldkey[:])
}

// Context shared by the answers

func (self *snCtfView) poolAnswer() SnCtfPool {
	return SnCtfPool{
		NoId:             self.Pool.NoId,
		PoolId:           self.Pool.PoolId,
		Vault:            snCtfAddress(self.Pool.Vault),
		CarryContract:    snCtfAddress(self.Pool.CarryContract),
		CarryColdkey:     snCtfCarryColdkey(self.Pool.CarryContract),
		FirstEpoch:       self.Pool.FirstEpoch,
		CountriesVersion: snCtfCountriesVersion(),
	}
}

// The estimate of when this epoch ends: its first block's time and 12 seconds
// a block, so it does not move during the epoch.
func (self *snCtfView) currentEpochAnswer() SnCtfCurrentEpoch {
	blocks := self.Clock.EndBlock + 1 - self.Clock.StartBlock
	drawAt := self.Clock.StartTime.Add(time.Duration(blocks) * snCtfBlockSeconds * time.Second)
	return SnCtfCurrentEpoch{Epoch: self.Clock.Epoch, StartBlock: self.Clock.StartBlock, EndBlock: self.Clock.EndBlock, DrawAt: snOperatorTime(drawAt)}
}

func snCtfRootAnswer(root *snCtfRoot) SnCtfRoot {
	return SnCtfRoot{
		Status:         root.status(),
		Seq:            root.Seq,
		RootIndex:      root.RootIndex,
		Epoch:          root.Epoch,
		LedgerHash:     snCtfRef(root.LedgerHash),
		CommittedBlock: root.CommitBlock,
		FinalizedBlock: root.FinalizedBlock,
	}
}

func (self *snCtfView) amountsAnswer(epoch *snCtfEpoch) SnCtfEpochAmounts {
	awarded := new(uint256.Int)
	for _, award := range epoch.Awards {
		awarded.Add(awarded, award.total())
	}
	carriedIn := new(uint256.Int)
	for _, pot := range epoch.Pots {
		if pot != nil {
			carriedIn.Add(carriedIn, &pot.In)
		}
	}
	pendingIn := self.pendingIn(epoch)
	return SnCtfEpochAmounts{
		Funding:     snCtfAmountOf(&epoch.Funding),
		Returned:    snCtfAmountOf(&epoch.Returned),
		Owed:        snCtfAmountOf(&epoch.Owed),
		Matured:     epoch.Matured,
		Fresh:       snCtfAmountOf(&epoch.Fresh),
		Slot:        snCtfAmountOf(&epoch.SlotBase),
		Awarded:     snCtfAmountOf(awarded),
		Paid:        snCtfAmountOf(snCtfSum(&epoch.PaidSlots, &epoch.PaidPots, &epoch.PaidPending)),
		PendingPaid: snCtfAmountOf(&epoch.PaidPending),
		PendingIn:   snCtfAmountOf(&pendingIn),
		Pending:     snCtfAmountOf(&epoch.PendingTotal),
		CarriedIn:   snCtfAmountOf(carriedIn),
		Carried:     snCtfAmountOf(&epoch.Carried),
	}
}

func snCtfSlotsOf(won int, total int) SnCtfSlots {
	return SnCtfSlots{Total: total, Won: won, Open: total - won}
}

func (self *snCtfView) lastEpochAnswer() *SnCtfLastEpoch {
	last := self.lastEpoch()
	if last == nil {
		return nil
	}
	return &SnCtfLastEpoch{
		Epoch:   last.Epoch,
		Drawn:   last.Drawn,
		Root:    snCtfRootAnswer(last.Root),
		Amounts: self.amountsAnswer(last),
		Slots:   snCtfSlotsOf(last.won(), snCtfSlots),
	}
}

func (self *snCtfView) pendingDrawAnswer() *SnCtfPendingDraw {
	if self.Pending == nil {
		return nil
	}
	answer := &SnCtfPendingDraw{Epoch: self.Pending.Epoch}
	if census := self.Pending.Census; census != nil {
		hash := snCtfRef(census.Hash)
		block, round := census.Block, census.Round
		answer.CensusHash, answer.CensusBlock, answer.Round = &hash, &block, &round
	}
	return answer
}

// The plain definitions every answer with estimates carries.
func snCtfDefinitions() SnCtfDefinitions {
	return SnCtfDefinitions{
		ChanceToWin: "A newcomer's chance to win one of a role's five slots in the next draw: min(1, 5 / entries), the draw's exact odds, where entries are last epoch's eligible units in the role plus the newcomer. 1 means a newcomer who meets the presence rule wins by default. Null without a draw last epoch.",
		WinEstimate: "What a newcomer who wins a slot of the role in the next draw would take: one slot at last epoch's slot amount plus a fifth of the role's pot, rounded down to the rao.",
		Available:   "An estimate of what a country's next draw holds: its provider and extender pots plus ten slots at last epoch's slot amount.",
		Eligibility: "Online, passing the operator's checks and in the country for at least half of the epoch, and active at its end.",
	}
}

func (self *snCtfView) links(country *snCtfCountry) SnCtfLinks {
	page := strings.TrimSuffix(self.PageUrl, "/")
	links := SnCtfLinks{Page: page, ChanceToWin: page + "#chance-to-win"}
	if country != nil {
		links.Page = page + "/" + country.Code
	}
	return links
}

// Countries

// snCtfChanceToWin is the draw's exact odds for a newcomer: min(1, 5/entries).
func snCtfChanceToWin(entries int64) float64 {
	if entries <= snCtfRanks {
		return 1
	}
	return float64(snCtfRanks) / float64(entries)
}

// One role of one country: last epoch's draw, and what the next draw holds.
func (self *snCtfView) roleState(index int, country *snCtfCountry, role int) SnCtfRoleState {
	last := self.lastEpoch()
	if last == nil {
		// before the first root: empty pots, no estimates, no draw figures
		return SnCtfRoleState{Pot: snCtfAmountOf(new(uint256.Int)), Awarded: snCtfAmountOf(new(uint256.Int))}
	}
	pot := last.pot(index, role)
	awarded := last.awarded(country.Code, role)
	fifth := new(uint256.Int).Div(&pot.Out, uint256.NewInt(snCtfRanks))
	state := SnCtfRoleState{
		Pot:         snCtfAmountOf(&pot.Out),
		Awarded:     snCtfAmountOf(awarded),
		WinEstimate: snCtfAmountPointer(snCtfSum(&last.SlotBase, fifth)),
	}
	if last.Drawn {
		slots := snCtfSlotsOf(pot.Winners, snCtfRanks)
		units := int64(pot.Units)
		nearMisses := pot.NearMisses
		entries := units + 1
		chance := snCtfChanceToWin(entries)
		state.Slots, state.Units, state.NearMisses, state.Entries, state.ChanceToWin = &slots, &units, &nearMisses, &entries, &chance
	}
	return state
}

// openEpochs counts the consecutive covered epochs, up to and including last
// epoch, with no winner in the country in either role.
func (self *snCtfView) openEpochs(index int) int {
	open := 0
	for i := len(self.Epochs) - 1; 0 <= i; i -= 1 {
		epoch := self.Epochs[i]
		if 0 < epoch.pot(index, snCtfRoleProvider).Winners || 0 < epoch.pot(index, snCtfRoleExtender).Winners {
			break
		}
		open += 1
	}
	return open
}

func (self *snCtfView) countryState(index int, country *snCtfCountry) *SnCtfCountryState {
	providers := self.roleState(index, country, snCtfRoleProvider)
	extenders := self.roleState(index, country, snCtfRoleExtender)
	state := &SnCtfCountryState{
		Code:         country.Code,
		Name:         country.Name,
		SentenceName: country.SentenceName,
		Inhabited:    country.Inhabited,
		OpenEpochs:   self.openEpochs(index),
		Awarded:      snCtfAmountOf(new(uint256.Int)),
		Providers:    providers,
		Extenders:    extenders,
	}
	if last := self.lastEpoch(); last != nil {
		state.Awarded = snCtfAmountOf(snCtfSum(last.awarded(country.Code, snCtfRoleProvider), last.awarded(country.Code, snCtfRoleExtender)))
		available := snCtfSum(&last.pot(index, snCtfRoleProvider).Out, &last.pot(index, snCtfRoleExtender).Out, new(uint256.Int).Mul(&last.SlotBase, uint256.NewInt(2*snCtfRanks)))
		state.Available = snCtfAmountPointer(available)
	}
	if providers.Slots != nil && extenders.Slots != nil {
		nearMisses := *providers.NearMisses + *extenders.NearMisses
		state.NearMisses = &nearMisses
		state.FlagHeld = providers.Slots.Open == 0 && extenders.Slots.Open == 0
	}
	return state
}

func (self *snCtfView) countryStates() []*SnCtfCountryState {
	states := make([]*SnCtfCountryState, 0, snCtfCountryCount)
	for index, country := range snCtfCountries() {
		states = append(states, self.countryState(index, country))
	}
	return states
}

// The answers

// snCtfResultOf is GET /sn/ctf.
func snCtfResultOf(view *snCtfView) *SnCtfResult {
	countries := view.countryStates()
	totals := SnCtfTotals{Countries: len(countries)}
	var notFullyCaptured *SnCtfNotFullyCaptured
	if last := view.lastEpoch(); last != nil && last.Drawn {
		notFullyCaptured = &SnCtfNotFullyCaptured{}
	}
	for _, country := range countries {
		if country.FlagHeld {
			totals.FlagsHeld += 1
		}
		if notFullyCaptured != nil {
			if 0 < country.Providers.Slots.Open {
				notFullyCaptured.Providers += 1
			}
			if 0 < country.Extenders.Slots.Open {
				notFullyCaptured.Extenders += 1
			}
		}
	}
	totals.NotFullyCaptured = notFullyCaptured
	return &SnCtfResult{
		AsOf:         snOperatorTime(view.AsOf),
		Unit:         snCtfUnit(),
		Pool:         view.poolAnswer(),
		CurrentEpoch: view.currentEpochAnswer(),
		LastEpoch:    view.lastEpochAnswer(),
		PendingDraw:  view.pendingDrawAnswer(),
		Totals:       totals,
		Definitions:  snCtfDefinitions(),
		Links:        view.links(nil),
		Countries:    countries,
	}
}

// The top's roles: one, or both together.
const (
	snCtfTopRoleProvider = "provider"
	snCtfTopRoleExtender = "extender"
	snCtfTopRoleAny      = "any"
)

// snCtfTopOf is GET /sn/ctf/top: the countries where the role (or either role)
// was not fully captured at last epoch's draw, by pot descending, then code.
// Without a draw last epoch it lists none.
func snCtfTopOf(view *snCtfView, role string, limit int, includeUninhabited bool) *SnCtfTopResult {
	result := &SnCtfTopResult{
		AsOf:               snOperatorTime(view.AsOf),
		Unit:               snCtfUnit(),
		CurrentEpoch:       view.currentEpochAnswer(),
		LastEpoch:          view.lastEpochAnswer(),
		PendingDraw:        view.pendingDrawAnswer(),
		Role:               role,
		IncludeUninhabited: includeUninhabited,
		Definitions:        snCtfDefinitions(),
		Links:              view.links(nil),
		Countries:          []*SnCtfTopEntry{},
	}
	if last := view.lastEpoch(); last == nil || !last.Drawn {
		return result
	}
	type ranked struct {
		pot     uint64
		country *SnCtfCountryState
	}
	open := []*ranked{}
	for _, country := range view.countryStates() {
		if !country.Inhabited && !includeUninhabited {
			continue
		}
		providersOpen := 0 < country.Providers.Slots.Open
		extendersOpen := 0 < country.Extenders.Slots.Open
		switch role {
		case snCtfTopRoleProvider:
			if providersOpen {
				open = append(open, &ranked{country.Providers.Pot.Rao, country})
			}
		case snCtfTopRoleExtender:
			if extendersOpen {
				open = append(open, &ranked{country.Extenders.Pot.Rao, country})
			}
		default:
			// five winners empty a role's pot, so both pots together are the
			// open roles' pots
			if providersOpen || extendersOpen {
				open = append(open, &ranked{country.Providers.Pot.Rao + country.Extenders.Pot.Rao, country})
			}
		}
	}
	slices.SortStableFunc(open, func(a, b *ranked) int {
		if c := cmp.Compare(b.pot, a.pot); c != 0 {
			return c
		}
		return cmp.Compare(a.country.Code, b.country.Code)
	})
	for i, entry := range open[:min(limit, len(open))] {
		result.Countries = append(result.Countries, &SnCtfTopEntry{Rank: i + 1, OpenPot: snCtfAmountOf(new(uint256.Int).SetUint64(entry.pot)), Country: entry.country})
	}
	return result
}

// The lower median of the winners' awards: the ⌈W/2⌉-th smallest of W.
func snCtfMedianAward(awards []*snCtfAward) *SnCtfAmount {
	if len(awards) == 0 {
		return nil
	}
	totals := make([]*uint256.Int, len(awards))
	for i, award := range awards {
		totals[i] = award.total()
	}
	slices.SortFunc(totals, func(a, b *uint256.Int) int { return a.Cmp(b) })
	return snCtfAmountPointer(totals[(len(totals)+1)/2-1])
}

func (self *snCtfView) historyRole(epoch *snCtfEpoch, index int, country *snCtfCountry, role int) SnCtfHistoryRole {
	pot := epoch.pot(index, role)
	awards := epoch.awardsOf(country.Code, role)
	awarded := epoch.awarded(country.Code, role)
	history := SnCtfHistoryRole{
		Units:       int64(pot.Units),
		Slots:       snCtfSlotsOf(pot.Winners, snCtfRanks),
		PotIn:       snCtfAmountOf(&pot.In),
		PotOut:      snCtfAmountOf(&pot.Out),
		Awarded:     snCtfAmountOf(awarded),
		MedianAward: snCtfMedianAward(awards),
	}
	if 0 < pot.Units {
		share := float64(pot.Winners) / float64(pot.Units)
		history.WinningShare = &share
	}
	return history
}

// snCtfCountryResultOf is GET /sn/ctf/country/{cc}: the country's row and its
// history, newest first.
func snCtfCountryResultOf(view *snCtfView, country *snCtfCountry) *SnCtfCountryResult {
	index := snCtfCountryIndex()[country.Code]
	history := []*SnCtfHistoryEpoch{}
	for i := len(view.Epochs) - 1; 0 <= i; i -= 1 {
		epoch := view.Epochs[i]
		history = append(history, &SnCtfHistoryEpoch{
			Epoch:      epoch.Epoch,
			Drawn:      epoch.Drawn,
			RootStatus: epoch.Root.status(),
			Slot:       snCtfAmountOf(&epoch.SlotBase),
			Providers:  view.historyRole(epoch, index, country, snCtfRoleProvider),
			Extenders:  view.historyRole(epoch, index, country, snCtfRoleExtender),
		})
	}
	return &SnCtfCountryResult{
		AsOf:         snOperatorTime(view.AsOf),
		Unit:         snCtfUnit(),
		Pool:         view.poolAnswer(),
		CurrentEpoch: view.currentEpochAnswer(),
		LastEpoch:    view.lastEpochAnswer(),
		PendingDraw:  view.pendingDrawAnswer(),
		Definitions:  snCtfDefinitions(),
		Links:        view.links(country),
		Country:      view.countryState(index, country),
		History:      history,
	}
}

// The exclusion reasons, as the census numbers them (sn ctf.Reason) and as the
// API names them; and the order in which /sn/ctf/client names the first rule a
// miner failed (operator.yml CtfExclusionReason). The census's counts keep the
// census's own order (CtfExclusionCounts).
const (
	snCtfReasonInactive         = 1
	snCtfReasonOfflineAtEnd     = 2
	snCtfReasonNotPublic        = 3
	snCtfReasonNoLocation       = 4
	snCtfReasonOwnNetwork       = 5
	snCtfReasonHeadBound        = 6
	snCtfReasonNoWallet         = 7
	snCtfReasonDuplicateAddress = 8
	snCtfReasonCountryChanged   = 9
	snCtfReasonPresence         = 10
)

var snCtfReasonNames = map[int]string{
	snCtfReasonInactive:         "inactive",
	snCtfReasonOfflineAtEnd:     "offline_at_end",
	snCtfReasonNotPublic:        "not_public",
	snCtfReasonNoLocation:       "no_location",
	snCtfReasonOwnNetwork:       "own_network",
	snCtfReasonHeadBound:        "head_bound",
	snCtfReasonNoWallet:         "no_wallet",
	snCtfReasonDuplicateAddress: "duplicate_address",
	snCtfReasonCountryChanged:   "country_changed",
	snCtfReasonPresence:         "presence",
}

var snCtfApiReasonOrder = []int{
	snCtfReasonOfflineAtEnd, snCtfReasonNotPublic, snCtfReasonNoLocation, snCtfReasonInactive, snCtfReasonPresence,
	snCtfReasonCountryChanged, snCtfReasonDuplicateAddress, snCtfReasonNoWallet, snCtfReasonHeadBound, snCtfReasonOwnNetwork,
}

// The reason a client was not a candidate when no sample found it in the role.
const snCtfReasonNotInRole = "not_in_role"

// snCtfFirstFailed is the first rule a member failed in the API's order, from
// every rule's result (bit 1 << reason); 0 when it failed none.
func snCtfFirstFailed(failed uint32) int {
	for _, reason := range snCtfApiReasonOrder {
		if failed&(1<<reason) != 0 {
			return reason
		}
	}
	return 0
}

func (self *SnCtfExclusionCounts) add(reason int, count int64) {
	switch reason {
	case snCtfReasonOfflineAtEnd:
		self.OfflineAtEnd += count
	case snCtfReasonNotPublic:
		self.NotPublic += count
	case snCtfReasonNoLocation:
		self.NoLocation += count
	case snCtfReasonInactive:
		self.Inactive += count
	case snCtfReasonPresence:
		self.Presence += count
	case snCtfReasonCountryChanged:
		self.CountryChanged += count
	case snCtfReasonDuplicateAddress:
		self.DuplicateAddress += count
	case snCtfReasonNoWallet:
		self.NoWallet += count
	case snCtfReasonHeadBound:
		self.HeadBound += count
	case snCtfReasonOwnNetwork:
		self.OwnNetwork += count
	}
}

// snCtfCensusMember is one miner the presence samples found in a role during an
// epoch, as the census evaluated it.
type snCtfCensusMember struct {
	Role      int
	ClientId  server.Id
	Candidate bool
	// bit 1 << reason for every rule it failed
	Failed uint32
	// its own country at the epoch's end ("zz": none), and its unit's draw's
	// country when a candidate
	Country     string
	DrawCountry string
	// non-excused samples that found it present in Country
	PresentSamples int
	UnitFamily     int
}

// snCtfExclusion is a census exclusion row: a count of miners under the
// lowest-numbered reason that excluded them.
type snCtfExclusion struct {
	Role   int
	Reason int
	Count  int64
}

// snCtfDraw is an epoch's drand pulse and the seed it gave.
type snCtfDraw struct {
	Round      uint64
	Signature  []byte
	Randomness [32]byte
	Seed       [32]byte
}

// snCtfEpochDetail is what GET /sn/ctf/epoch/{epoch} adds to the view.
type snCtfEpochDetail struct {
	// the census's counts; zero without one
	Candidates     int64
	ExcusedSamples int
	// the census's exclusion rows: each miner counted once, under the
	// lowest-numbered reason that excluded it (sn ctf.Reason)
	Exclusions []*snCtfExclusion
	Draw       *snCtfDraw
	Pending    []*snCtfPendingAward
}

// excluded are the exclusion counts of the census's published exclusion
// object, by role and reason name: what anyone can check against the
// committed census, and the same for good, as an immutable answer must be.
// The object counts a miner that failed several rules under the census's
// lowest-numbered reason (sn ctf.Reason order), as operator.yml's
// CtfExclusionCounts says, which is not always the first of the
// CtfExclusionReason order /sn/ctf/client uses (presence before no_wallet);
// the census members that tell the two apart are pruned after 60 epochs.
func (self *snCtfEpochDetail) excluded() SnCtfExcluded {
	var excluded SnCtfExcluded
	for _, exclusion := range self.Exclusions {
		count := &excluded.Providers
		if exclusion.Role == snCtfRoleExtender {
			count = &excluded.Extenders
		}
		count.add(exclusion.Reason, exclusion.Count)
	}
	return excluded
}

// snCtfEpochResultOf is GET /sn/ctf/epoch/{epoch}, for a covered epoch.
func snCtfEpochResultOf(view *snCtfView, epoch *snCtfEpoch, detail *snCtfEpochDetail) *SnCtfEpochResult {
	result := &SnCtfEpochResult{
		AsOf:       snOperatorTime(epoch.Root.changeTime()),
		Unit:       snCtfUnit(),
		Pool:       view.poolAnswer(),
		Epoch:      epoch.Epoch,
		StartBlock: epoch.StartBlock,
		EndBlock:   epoch.EndBlock,
		Amounts:    view.amountsAnswer(epoch),
		Slots:      snCtfSlotsOf(epoch.won(), snCtfSlots),
		Root:       snCtfRootAnswer(epoch.Root),
		Objects: SnCtfEpochObjects{
			Header:    snCtfRef(epoch.Root.LedgerHash),
			Countries: snCtfCountriesVersion(),
			Awards:    snCtfRef(epoch.AwardsRef),
			Pots:      snCtfRef(epoch.PotsRef),
			Pending:   snCtfRef(epoch.PendingRef),
		},
		Countries:     []*SnCtfEpochCountry{},
		Awards:        []*SnCtfAward{},
		PendingAwards: []*SnCtfPendingAward{},
	}
	unitsProviders, unitsExtenders := int64(0), int64(0)
	for index, country := range snCtfCountries() {
		role := func(role int) SnCtfEpochRole {
			pot := epoch.pot(index, role)
			awarded, pending := new(uint256.Int), new(uint256.Int)
			for _, award := range epoch.awardsOf(country.Code, role) {
				awarded.Add(awarded, award.total())
				if award.Deferred {
					pending.Add(pending, &award.Pot)
				}
			}
			nearMisses := int64(0)
			if epoch.Drawn {
				// without a draw there is no basis for a near miss
				nearMisses = pot.NearMisses
			}
			return SnCtfEpochRole{
				Units:      int64(pot.Units),
				Slots:      snCtfSlotsOf(pot.Winners, snCtfRanks),
				NearMisses: nearMisses,
				PotIn:      snCtfAmountOf(&pot.In),
				PotOut:     snCtfAmountOf(&pot.Out),
				Awarded:    snCtfAmountOf(awarded),
				Pending:    snCtfAmountOf(pending),
			}
		}
		row := &SnCtfEpochCountry{Code: country.Code, Name: country.Name, Providers: role(snCtfRoleProvider), Extenders: role(snCtfRoleExtender)}
		unitsProviders += row.Providers.Units
		unitsExtenders += row.Extenders.Units
		result.Countries = append(result.Countries, row)
	}
	if census := epoch.Census; census != nil {
		hash := snCtfRef(census.Hash)
		result.Objects.Census = &hash
		result.Census = &SnCtfCensus{
			Hash:           hash,
			Block:          census.Block,
			Candidates:     detail.Candidates,
			Units:          SnCtfRoleCounts{Providers: unitsProviders, Extenders: unitsExtenders},
			ExcusedSamples: int64(detail.ExcusedSamples),
			Excluded:       detail.excluded(),
		}
	}
	if draw := detail.Draw; draw != nil && epoch.Drawn {
		result.Draw = &SnCtfDraw{Round: draw.Round, Signature: snCtfHex(draw.Signature), Randomness: snCtfHex(draw.Randomness[:]), Seed: snCtfHex(draw.Seed[:])}
	}
	awards := slices.Clone(epoch.Awards)
	slices.SortFunc(awards, func(a, b *snCtfAward) int {
		return cmp.Or(cmp.Compare(a.Role, b.Role), cmp.Compare(a.Country, b.Country), cmp.Compare(a.Rank, b.Rank))
	})
	for _, award := range awards {
		result.Awards = append(result.Awards, &SnCtfAward{
			Role:        snCtfRoleName(award.Role),
			Country:     award.Country,
			Rank:        award.Rank,
			UnitHash:    snCtfHex(award.Unit[:]),
			UnitFamily:  award.UnitFamily,
			ClientId:    award.ClientId.String(),
			Coldkey:     snCtfHex(award.Coldkey[:]),
			ColdkeySs58: snCtfColdkeySs58(award.Coldkey),
			Slot:        snCtfAmountOf(&award.Slot),
			PotShare:    snCtfAmountOf(&award.Pot),
			Pending:     award.Deferred,
		})
	}
	pending := slices.Clone(detail.Pending)
	slices.SortFunc(pending, func(a, b *snCtfPendingAward) int {
		return cmp.Or(cmp.Compare(a.AwardEpoch, b.AwardEpoch), cmp.Compare(a.Role, b.Role), cmp.Compare(a.Country, b.Country), cmp.Compare(a.Rank, b.Rank))
	})
	for _, award := range pending {
		result.PendingAwards = append(result.PendingAwards, &SnCtfPendingAward{
			AwardEpoch:  award.AwardEpoch,
			Role:        snCtfRoleName(award.Role),
			Country:     award.Country,
			Rank:        award.Rank,
			Coldkey:     snCtfHex(award.Coldkey[:]),
			ColdkeySs58: snCtfColdkeySs58(award.Coldkey),
			Amount:      snCtfAmountOf(&award.Amount),
		})
	}
	return result
}

// snCtfClientEpochFacts is a client's census members and awards in one epoch.
type snCtfClientEpochFacts struct {
	// by role; nil when no sample found it in the role
	Members map[int]*snCtfCensusMember
	Awards  map[int]*snCtfAward
	// the census's half-epoch rule
	RequiredSamples int
	ExcusedSamples  int
}

func (self *snCtfView) clientRole(epoch *snCtfEpoch, facts *snCtfClientEpochFacts, role int) SnCtfClientRole {
	answer := SnCtfClientRole{}
	member := facts.Members[role]
	if member == nil {
		reason := snCtfReasonNotInRole
		answer.Reason = &reason
		return answer
	}
	answer.Candidate = member.Candidate
	answer.Presence = &SnCtfPresence{Present: member.PresentSamples, Required: facts.RequiredSamples, Samples: snCtfSamplesPerEpoch, Excused: facts.ExcusedSamples}
	if member.UnitFamily == 4 || member.UnitFamily == 6 {
		family := member.UnitFamily
		answer.UnitFamily = &family
	}
	country := member.Country
	if member.Candidate {
		country = member.DrawCountry
		// a draw that ran holds its unit
		if index, ok := snCtfCountryIndex()[country]; ok && epoch.Drawn && 0 < epoch.pot(index, role).Units {
			units := int64(epoch.pot(index, role).Units)
			answer.UnitsInDraw = &units
		}
	} else if reason, ok := snCtfReasonNames[snCtfFirstFailed(member.Failed)]; ok {
		answer.Reason = &reason
	} else {
		// a member left out by no rule the census names reads as not in the role
		reason := snCtfReasonNotInRole
		answer.Reason = &reason
	}
	if _, ok := snCtfCountryIndex()[country]; ok {
		answer.Country = &country
	}
	if award := facts.Awards[role]; award != nil {
		rank := award.Rank
		coldkey, coldkeySs58 := snCtfHex(award.Coldkey[:]), snCtfColdkeySs58(award.Coldkey)
		answer.Won, answer.Rank, answer.Pending = true, &rank, award.Deferred
		answer.Slot, answer.PotShare = snCtfAmountPointer(&award.Slot), snCtfAmountPointer(&award.Pot)
		answer.Coldkey, answer.ColdkeySs58 = &coldkey, &coldkeySs58
	}
	return answer
}

// snCtfClientResultOf is GET /sn/ctf/client: the client's status in the most
// recent limit covered epochs, newest first.
func snCtfClientResultOf(view *snCtfView, clientId server.Id, facts map[uint64]*snCtfClientEpochFacts, limit int) *SnCtfClientResult {
	result := &SnCtfClientResult{AsOf: snOperatorTime(view.AsOf), Unit: snCtfUnit(), ClientId: clientId.String(), Epochs: []*SnCtfClientEpoch{}}
	for i := len(view.Epochs) - 1; 0 <= i && len(result.Epochs) < limit; i -= 1 {
		epoch := view.Epochs[i]
		epochFacts := facts[epoch.Epoch]
		if epochFacts == nil {
			epochFacts = &snCtfClientEpochFacts{}
		}
		result.Epochs = append(result.Epochs, &SnCtfClientEpoch{
			Epoch:      epoch.Epoch,
			Drawn:      epoch.Drawn,
			RootStatus: epoch.Root.status(),
			Providers:  view.clientRole(epoch, epochFacts, snCtfRoleProvider),
			Extenders:  view.clientRole(epoch, epochFacts, snCtfRoleExtender),
		})
	}
	return result
}
