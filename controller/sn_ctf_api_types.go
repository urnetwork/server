package controller

// sn_ctf_api_types.go — the answers of the capture the flag routes, field for
// field as sn/api/operator.yml documents them (api/spec_conformance_test.go
// holds them to it). A field the document makes nullable is a pointer here.

// SnCtfUnit is the unit of every amount of an answer.
type SnCtfUnit struct {
	Token    string `json:"token"`
	Symbol   string `json:"symbol"`
	Netuid   int    `json:"netuid"`
	Decimals int    `json:"decimals"`
}

// SnCtfAmount is an exact amount: alpha with nine fractional digits, and rao.
type SnCtfAmount struct {
	Alpha string `json:"alpha"`
	Rao   uint64 `json:"rao"`
}

// SnCtfPool is the capture the flag pool's identity on chain.
type SnCtfPool struct {
	NoId             uint64 `json:"no_id"`
	PoolId           uint64 `json:"pool_id"`
	Vault            string `json:"vault"`
	CarryContract    string `json:"carry_contract"`
	CarryColdkey     string `json:"carry_coldkey"`
	FirstEpoch       uint64 `json:"first_epoch"`
	CountriesVersion string `json:"countries_version"`
}

// SnCtfCurrentEpoch is this epoch, whose draw follows its end.
type SnCtfCurrentEpoch struct {
	Epoch      uint64 `json:"epoch"`
	StartBlock uint64 `json:"start_block"`
	EndBlock   uint64 `json:"end_block"`
	DrawAt     string `json:"draw_at"`
}

// SnCtfRoot is the CTF root that covers an epoch.
type SnCtfRoot struct {
	Status         string  `json:"status"`
	Seq            uint64  `json:"seq"`
	RootIndex      *uint64 `json:"root_index"`
	Epoch          uint64  `json:"epoch"`
	LedgerHash     string  `json:"ledger_hash"`
	CommittedBlock uint64  `json:"committed_block"`
	FinalizedBlock *uint64 `json:"finalized_block"`
}

// SnCtfEpochAmounts are one epoch's amounts.
type SnCtfEpochAmounts struct {
	Funding     SnCtfAmount `json:"funding"`
	Returned    SnCtfAmount `json:"returned"`
	Owed        SnCtfAmount `json:"owed"`
	Matured     bool        `json:"matured"`
	Fresh       SnCtfAmount `json:"fresh"`
	Slot        SnCtfAmount `json:"slot"`
	Awarded     SnCtfAmount `json:"awarded"`
	Paid        SnCtfAmount `json:"paid"`
	PendingPaid SnCtfAmount `json:"pending_paid"`
	PendingIn   SnCtfAmount `json:"pending_in"`
	Pending     SnCtfAmount `json:"pending"`
	CarriedIn   SnCtfAmount `json:"carried_in"`
	Carried     SnCtfAmount `json:"carried"`
}

// SnCtfSlots are slots as counted for a draw: Won + Open = Total.
type SnCtfSlots struct {
	Total int `json:"total"`
	Won   int `json:"won"`
	Open  int `json:"open"`
}

// SnCtfLastEpoch is the latest epoch with a committed CTF root.
type SnCtfLastEpoch struct {
	Epoch   uint64            `json:"epoch"`
	Drawn   bool              `json:"drawn"`
	Root    SnCtfRoot         `json:"root"`
	Amounts SnCtfEpochAmounts `json:"amounts"`
	Slots   SnCtfSlots        `json:"slots"`
}

// SnCtfPendingDraw is an ended epoch whose root is not committed yet. The
// census and round fields are null together until the census is committed.
type SnCtfPendingDraw struct {
	Epoch       uint64  `json:"epoch"`
	CensusHash  *string `json:"census_hash"`
	CensusBlock *uint64 `json:"census_block"`
	Round       *uint64 `json:"round"`
}

// SnCtfRoleState is one role in one country: last epoch's draw, and what the
// next draw holds. The draw figures are nil together without a draw.
type SnCtfRoleState struct {
	Slots       *SnCtfSlots  `json:"slots"`
	Units       *int64       `json:"units"`
	NearMisses  *int64       `json:"near_misses"`
	Pot         SnCtfAmount  `json:"pot"`
	Awarded     SnCtfAmount  `json:"awarded"`
	WinEstimate *SnCtfAmount `json:"win_estimate"`
	Entries     *int64       `json:"entries"`
	ChanceToWin *float64     `json:"chance_to_win"`
}

// SnCtfCountryState is one country's row.
type SnCtfCountryState struct {
	Code         string         `json:"code"`
	Name         string         `json:"name"`
	SentenceName string         `json:"sentence_name"`
	Inhabited    bool           `json:"inhabited"`
	FlagHeld     bool           `json:"flag_held"`
	OpenEpochs   int            `json:"open_epochs"`
	NearMisses   *int64         `json:"near_misses"`
	Available    *SnCtfAmount   `json:"available"`
	Awarded      SnCtfAmount    `json:"awarded"`
	Providers    SnCtfRoleState `json:"providers"`
	Extenders    SnCtfRoleState `json:"extenders"`
}

// SnCtfNotFullyCaptured counts the countries where a role was not fully
// captured at last epoch's draw.
type SnCtfNotFullyCaptured struct {
	Providers int `json:"providers"`
	Extenders int `json:"extenders"`
}

// SnCtfTotals are counts over the 249 countries.
type SnCtfTotals struct {
	Countries        int                    `json:"countries"`
	FlagsHeld        int                    `json:"flags_held"`
	NotFullyCaptured *SnCtfNotFullyCaptured `json:"not_fully_captured"`
}

// SnCtfDefinitions are plain definitions of the estimates an answer carries.
type SnCtfDefinitions struct {
	ChanceToWin string `json:"chance_to_win"`
	WinEstimate string `json:"win_estimate"`
	Available   string `json:"available"`
	Eligibility string `json:"eligibility"`
}

// SnCtfLinks are the pages for people.
type SnCtfLinks struct {
	Page        string `json:"page"`
	ChanceToWin string `json:"chance_to_win"`
}

// SnCtfResult is the answer of GET /sn/ctf.
type SnCtfResult struct {
	AsOf         string               `json:"as_of"`
	Unit         SnCtfUnit            `json:"unit"`
	Pool         SnCtfPool            `json:"pool"`
	CurrentEpoch SnCtfCurrentEpoch    `json:"current_epoch"`
	LastEpoch    *SnCtfLastEpoch      `json:"last_epoch"`
	PendingDraw  *SnCtfPendingDraw    `json:"pending_draw"`
	Totals       SnCtfTotals          `json:"totals"`
	Definitions  SnCtfDefinitions     `json:"definitions"`
	Links        SnCtfLinks           `json:"links"`
	Countries    []*SnCtfCountryState `json:"countries"`
}

// SnCtfHereResult is the answer of GET /sn/ctf/here.
type SnCtfHereResult struct {
	Country *string `json:"country"`
}

// SnCtfHistoryRole is one role in one country in one epoch.
type SnCtfHistoryRole struct {
	Units        int64        `json:"units"`
	Slots        SnCtfSlots   `json:"slots"`
	PotIn        SnCtfAmount  `json:"pot_in"`
	PotOut       SnCtfAmount  `json:"pot_out"`
	Awarded      SnCtfAmount  `json:"awarded"`
	MedianAward  *SnCtfAmount `json:"median_award"`
	WinningShare *float64     `json:"winning_share"`
}

// SnCtfHistoryEpoch is one country in one epoch.
type SnCtfHistoryEpoch struct {
	Epoch      uint64           `json:"epoch"`
	Drawn      bool             `json:"drawn"`
	RootStatus string           `json:"root_status"`
	Slot       SnCtfAmount      `json:"slot"`
	Providers  SnCtfHistoryRole `json:"providers"`
	Extenders  SnCtfHistoryRole `json:"extenders"`
}

// SnCtfCountryResult is the answer of GET /sn/ctf/country/{cc}.
type SnCtfCountryResult struct {
	AsOf         string               `json:"as_of"`
	Unit         SnCtfUnit            `json:"unit"`
	Pool         SnCtfPool            `json:"pool"`
	CurrentEpoch SnCtfCurrentEpoch    `json:"current_epoch"`
	LastEpoch    *SnCtfLastEpoch      `json:"last_epoch"`
	PendingDraw  *SnCtfPendingDraw    `json:"pending_draw"`
	Definitions  SnCtfDefinitions     `json:"definitions"`
	Links        SnCtfLinks           `json:"links"`
	Country      *SnCtfCountryState   `json:"country"`
	History      []*SnCtfHistoryEpoch `json:"history"`
}

// SnCtfTopEntry is one country of GET /sn/ctf/top.
type SnCtfTopEntry struct {
	Rank    int                `json:"rank"`
	OpenPot SnCtfAmount        `json:"open_pot"`
	Country *SnCtfCountryState `json:"country"`
}

// SnCtfTopResult is the answer of GET /sn/ctf/top.
type SnCtfTopResult struct {
	AsOf               string            `json:"as_of"`
	Unit               SnCtfUnit         `json:"unit"`
	CurrentEpoch       SnCtfCurrentEpoch `json:"current_epoch"`
	LastEpoch          *SnCtfLastEpoch   `json:"last_epoch"`
	PendingDraw        *SnCtfPendingDraw `json:"pending_draw"`
	Role               string            `json:"role"`
	IncludeUninhabited bool              `json:"include_uninhabited"`
	Definitions        SnCtfDefinitions  `json:"definitions"`
	Links              SnCtfLinks        `json:"links"`
	Countries          []*SnCtfTopEntry  `json:"countries"`
}

// SnCtfExclusionCounts are the miners each rule left out of a role's census.
type SnCtfExclusionCounts struct {
	OfflineAtEnd     int64 `json:"offline_at_end"`
	NotPublic        int64 `json:"not_public"`
	NoLocation       int64 `json:"no_location"`
	Inactive         int64 `json:"inactive"`
	Presence         int64 `json:"presence"`
	CountryChanged   int64 `json:"country_changed"`
	DuplicateAddress int64 `json:"duplicate_address"`
	NoWallet         int64 `json:"no_wallet"`
	HeadBound        int64 `json:"head_bound"`
	OwnNetwork       int64 `json:"own_network"`
}

// SnCtfRoleCounts is a count per role.
type SnCtfRoleCounts struct {
	Providers int64 `json:"providers"`
	Extenders int64 `json:"extenders"`
}

// SnCtfExcluded are the exclusion counts of both roles.
type SnCtfExcluded struct {
	Providers SnCtfExclusionCounts `json:"providers"`
	Extenders SnCtfExclusionCounts `json:"extenders"`
}

// SnCtfCensus is a census commitment and its counts.
type SnCtfCensus struct {
	Hash           string          `json:"hash"`
	Block          uint64          `json:"block"`
	Candidates     int64           `json:"candidates"`
	Units          SnCtfRoleCounts `json:"units"`
	ExcusedSamples int64           `json:"excused_samples"`
	Excluded       SnCtfExcluded   `json:"excluded"`
}

// SnCtfDraw is a draw's randomness.
type SnCtfDraw struct {
	Round      uint64 `json:"round"`
	Signature  string `json:"signature"`
	Randomness string `json:"randomness"`
	Seed       string `json:"seed"`
}

// SnCtfEpochObjects are an epoch's ledger objects.
type SnCtfEpochObjects struct {
	Header    string  `json:"header"`
	Countries string  `json:"countries"`
	Census    *string `json:"census"`
	Awards    string  `json:"awards"`
	Pots      string  `json:"pots"`
	Pending   string  `json:"pending"`
}

// SnCtfEpochRole is one role in one country in an epoch.
type SnCtfEpochRole struct {
	Units      int64       `json:"units"`
	Slots      SnCtfSlots  `json:"slots"`
	NearMisses int64       `json:"near_misses"`
	PotIn      SnCtfAmount `json:"pot_in"`
	PotOut     SnCtfAmount `json:"pot_out"`
	Awarded    SnCtfAmount `json:"awarded"`
	Pending    SnCtfAmount `json:"pending"`
}

// SnCtfEpochCountry is one country in an epoch.
type SnCtfEpochCountry struct {
	Code      string         `json:"code"`
	Name      string         `json:"name"`
	Providers SnCtfEpochRole `json:"providers"`
	Extenders SnCtfEpochRole `json:"extenders"`
}

// SnCtfAward is one winner, as its award row records it.
type SnCtfAward struct {
	Role        string      `json:"role"`
	Country     string      `json:"country"`
	Rank        int         `json:"rank"`
	UnitHash    string      `json:"unit_hash"`
	UnitFamily  int         `json:"unit_family"`
	ClientId    string      `json:"client_id"`
	Coldkey     string      `json:"coldkey"`
	ColdkeySs58 string      `json:"coldkey_ss58"`
	Slot        SnCtfAmount `json:"slot"`
	PotShare    SnCtfAmount `json:"pot_share"`
	Pending     bool        `json:"pending"`
}

// SnCtfPendingAward is a pot share still owed.
type SnCtfPendingAward struct {
	AwardEpoch  uint64      `json:"award_epoch"`
	Role        string      `json:"role"`
	Country     string      `json:"country"`
	Rank        int         `json:"rank"`
	Coldkey     string      `json:"coldkey"`
	ColdkeySs58 string      `json:"coldkey_ss58"`
	Amount      SnCtfAmount `json:"amount"`
}

// SnCtfEpochResult is the answer of GET /sn/ctf/epoch/{epoch}.
type SnCtfEpochResult struct {
	AsOf          string               `json:"as_of"`
	Unit          SnCtfUnit            `json:"unit"`
	Pool          SnCtfPool            `json:"pool"`
	Epoch         uint64               `json:"epoch"`
	StartBlock    uint64               `json:"start_block"`
	EndBlock      uint64               `json:"end_block"`
	Census        *SnCtfCensus         `json:"census"`
	Draw          *SnCtfDraw           `json:"draw"`
	Amounts       SnCtfEpochAmounts    `json:"amounts"`
	Slots         SnCtfSlots           `json:"slots"`
	Root          SnCtfRoot            `json:"root"`
	Objects       SnCtfEpochObjects    `json:"objects"`
	Countries     []*SnCtfEpochCountry `json:"countries"`
	Awards        []*SnCtfAward        `json:"awards"`
	PendingAwards []*SnCtfPendingAward `json:"pending_awards"`
}

// SnCtfPresence is a client's presence samples in a role.
type SnCtfPresence struct {
	Present  int `json:"present"`
	Required int `json:"required"`
	Samples  int `json:"samples"`
	Excused  int `json:"excused"`
}

// SnCtfClientRole is the client in one role in one epoch.
type SnCtfClientRole struct {
	Candidate   bool           `json:"candidate"`
	Reason      *string        `json:"reason"`
	Country     *string        `json:"country"`
	UnitFamily  *int           `json:"unit_family"`
	Presence    *SnCtfPresence `json:"presence"`
	UnitsInDraw *int64         `json:"units_in_draw"`
	Won         bool           `json:"won"`
	Rank        *int           `json:"rank"`
	Slot        *SnCtfAmount   `json:"slot"`
	PotShare    *SnCtfAmount   `json:"pot_share"`
	Pending     bool           `json:"pending"`
	Coldkey     *string        `json:"coldkey"`
	ColdkeySs58 *string        `json:"coldkey_ss58"`
}

// SnCtfClientEpoch is the client in one epoch.
type SnCtfClientEpoch struct {
	Epoch      uint64          `json:"epoch"`
	Drawn      bool            `json:"drawn"`
	RootStatus string          `json:"root_status"`
	Providers  SnCtfClientRole `json:"providers"`
	Extenders  SnCtfClientRole `json:"extenders"`
}

// SnCtfClientResult is the answer of GET /sn/ctf/client.
type SnCtfClientResult struct {
	AsOf     string              `json:"as_of"`
	Unit     SnCtfUnit           `json:"unit"`
	ClientId string              `json:"client_id"`
	Epochs   []*SnCtfClientEpoch `json:"epochs"`
}
