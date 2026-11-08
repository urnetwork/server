package model

import (
	"fmt"
	"math"
	"time"

	"github.com/urnetwork/glog"
	"gopkg.in/yaml.v3"
)

// Tiers and sidecar limits (DESIGN.md 20.4, 20.5): pro.yml's free.sidecar, pro.sidecar,
// team: and sidecar_abuse: blocks, and the one table of their keys.
//
// Every limit is read by its key, the same limit_key that QuotaRefusal, QuotaLimits and
// network_sidecar_limit carry, through proSidecarLimitRows. So ProSidecarLimits.Limit
// and QuotaLimits.Limits cannot drift, and adding a key is one row plus its pro.yml
// value. Bytes read as bytes, a true as 1 and a false as 0, a duration as milliseconds,
// and `none` as -1, which is QuotaLimits' reading of each. A key no row names reads 0,
// which is "not included", so a limit this build does not know fails closed.
//
// Unusable values follow ProConfig's doc: a missing or garbage value logs, reads zero and
// never panics, and each read keys off the value, never off a flag saying pro.yml loaded.
// Zero, once enforced, means "not included": it refuses creation, and never activation,
// reads, deletes, erasure, export or maintenance (D-QUOTA-2). EnforceSidecarLimits is the
// only switch, so an absent pro.yml stays inert.
//
// A parsed config is read-only after Pro() returns it, and safe for concurrent use.

// A network's plan tier. Tiers resolve only from in-window balances (D-QUOTA-1).
type NetworkTier string

const (
	// Community
	NetworkTierFree NetworkTier = "free"
	NetworkTierPro  NetworkTier = "pro"
	NetworkTierTeam NetworkTier = "team"
)

// How a key's yaml value reads. `none` is a value, not a kind: counts, rates and bytes
// take it, and it reads proSidecarLimitNone.
type proSidecarKind int

const (
	// a whole number of things
	proSidecarKindCount proSidecarKind = iota
	// a whole number of things per window, so a refusal has a window that resets
	proSidecarKindRate
	// ParseByteCount's syntax, or a whole number of bytes
	proSidecarKindBytes
	proSidecarKindBool
	// time.ParseDuration's syntax
	proSidecarKindDuration
)

// Where a key lives in pro.yml.
type proSidecarBlock int

const (
	// each tier's sidecar: block: quotas, which grow with the tier
	proSidecarBlockTier proSidecarBlock = iota
	// sidecar_abuse:, equal on every tier and never sold (D-QUOTA-7)
	proSidecarBlockAbuse
	// the top level of the file, equal on every tier
	proSidecarBlockTop
)

const (
	// What `none` reads, as sidecar.QuotaLimits defines it.
	proSidecarLimitNone = int64(-1)

	proSidecarNoneYaml = "none"

	// One full i/ re-key: the largest padded i/ segment or index file (§15.7), so an
	// allowance of at least this covers one closure upload (owner decision 146).
	proSidecarClosureFloorMinByteCount = ByteCount(1088 * 1024)

	// A network's closure pacing, fixed by owner decision 146. §20.7's wait bounds are
	// stated at this rate, so nothing takes it lower, and zero would stop erasure.
	proSidecarClosureUploadsMinPerS = int64(4)
)

// One limit key.
type proSidecarLimitRow struct {
	key   string
	block proSidecarBlock
	kind  proSidecarKind
	// No network_sidecar_limit row may set it (owner decision 146).
	fixed bool
	// When positive, the key reads at least this, whatever pro.yml says, since an
	// unusable value here would stop erasure rather than creation.
	minValue int64
	// When set, the key reads at least that key's reading, unless it reads `none`.
	minValueKey string
}

// Every limit key, in pro.yml's order. DESIGN.md 20.4 is canonical for the tier keys, and
// each area adds its abuse keys here. An array, so that its length sizes
// proSidecarLimitValues.
var proSidecarLimitRows = [...]proSidecarLimitRow{
	{key: "evs_unpaid_read_rate", block: proSidecarBlockTop, kind: proSidecarKindBytes},

	{key: "storage", block: proSidecarBlockTier, kind: proSidecarKindBytes},
	{key: "sidecar_clients", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "top_level_client_limit", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "dns_entries", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "tls_endpoints", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "config_admissions", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "config_statements_per_min", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "config_statements_burst", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "config_statements_per_day", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "config_bases_per_min", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "config_bases_per_day", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "store_weight", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "evs_drops_per_day", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "acl_rules", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "acl_groups", block: proSidecarBlockTier, kind: proSidecarKindBool},
	{key: "acl_requests", block: proSidecarBlockTier, kind: proSidecarKindBool},
	{key: "acl_approvers_two", block: proSidecarBlockTier, kind: proSidecarKindBool},
	{key: "acl_network_shares", block: proSidecarBlockTier, kind: proSidecarKindBool},
	{key: "smtp_connections_per_day", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "smtp_bytes_per_day", block: proSidecarBlockTier, kind: proSidecarKindBytes},
	{key: "smtp_recipients_per_message", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "smtp_recipients_per_day", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "smtp_message_bytes", block: proSidecarBlockTier, kind: proSidecarKindBytes},
	{key: "smtp_burst", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "smtp_burst_window", block: proSidecarBlockTier, kind: proSidecarKindDuration},
	{key: "smtp_concurrent_connections", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "mail_inbound_message_bytes", block: proSidecarBlockTier, kind: proSidecarKindBytes},
	{key: "mail_burners_active", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "mail_burner_creates_per_day", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "tls_endpoint_concurrent", block: proSidecarBlockTier, kind: proSidecarKindCount},
	{key: "tls_endpoint_new_per_s", block: proSidecarBlockTier, kind: proSidecarKindRate},
	{key: "tls_endpoint_mbit_per_s", block: proSidecarBlockTier, kind: proSidecarKindRate},

	{key: "mail_group_members", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "mail_inbound_recipients", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "mail_inbound_repos_per_message", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "probation_dns_entries", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{
		key:         "evs_closure_bytes_per_day",
		block:       proSidecarBlockAbuse,
		kind:        proSidecarKindBytes,
		minValueKey: "evs_closure_bytes_floor",
	},
	{
		key:      "evs_closure_bytes_floor",
		block:    proSidecarBlockAbuse,
		kind:     proSidecarKindBytes,
		fixed:    true,
		minValue: proSidecarClosureFloorMinByteCount,
	},
	{
		key:      "evs_closure_uploads_per_s",
		block:    proSidecarBlockAbuse,
		kind:     proSidecarKindRate,
		fixed:    true,
		minValue: proSidecarClosureUploadsMinPerS,
	},
	{key: "evs_drop_previews_per_day", block: proSidecarBlockAbuse, kind: proSidecarKindRate},
	{key: "evs_subscribers_per_repo", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "evs_repos_per_client", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "evs_read_receipts_per_repo_day", block: proSidecarBlockAbuse, kind: proSidecarKindRate},
	{key: "evs_read_receipt_bytes", block: proSidecarBlockAbuse, kind: proSidecarKindBytes},
	{key: "dns_changes_per_hour", block: proSidecarBlockAbuse, kind: proSidecarKindRate},
	{key: "dns_challenges_per_name_hour", block: proSidecarBlockAbuse, kind: proSidecarKindRate},
	{key: "dns_dkim_selectors", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "tls_visitor_concurrent", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "tls_visitor_new_per_s", block: proSidecarBlockAbuse, kind: proSidecarKindRate},
	{key: "tls_visitor_aggregate_multiple", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "smtp_community_connections_per_hour", block: proSidecarBlockAbuse, kind: proSidecarKindRate},
	{key: "smtp_community_median_multiple", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "smtp_destination_connections_per_hour", block: proSidecarBlockAbuse, kind: proSidecarKindRate},
	{key: "smtp_destination_concurrency", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "smtp_hard_bounce_percent", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "smtp_hard_bounce_floor", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "directory_requests_pending", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "directory_requests_per_day", block: proSidecarBlockAbuse, kind: proSidecarKindRate},
	{key: "directory_requests_per_run", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "directory_target_pending", block: proSidecarBlockAbuse, kind: proSidecarKindCount},
	{key: "directory_card_bytes", block: proSidecarBlockAbuse, kind: proSidecarKindBytes},
}

var limitKeyProSidecarRowIndexes = func() map[string]int {
	limitKeyRowIndexes := make(map[string]int, len(proSidecarLimitRows))
	for rowIndex := range proSidecarLimitRows {
		limitKeyRowIndexes[proSidecarLimitRows[rowIndex].key] = rowIndex
	}
	return limitKeyRowIndexes
}()

func proSidecarLimitRowForKey(limitKey string) (*proSidecarLimitRow, bool) {
	rowIndex, ok := limitKeyProSidecarRowIndexes[limitKey]
	if !ok {
		return nil, false
	}
	return &proSidecarLimitRows[rowIndex], true
}

// One value per row, by the row's index. Plain values, so limits copy and compare as
// ProTier's other fields do, and zero is what an absent pro.yml reads.
type proSidecarLimitValues [len(proSidecarLimitRows)]int64

// A key's reading, after the row's minimums.
func (self *proSidecarLimitValues) limit(limitKey string) int64 {
	rowIndex, ok := limitKeyProSidecarRowIndexes[limitKey]
	if !ok {
		return 0
	}
	row := &proSidecarLimitRows[rowIndex]
	value := self[rowIndex]
	if 0 < row.minValue {
		value = max(value, row.minValue)
	}
	if row.minValueKey != "" && value != proSidecarLimitNone {
		value = max(value, self.limit(row.minValueKey))
	}
	return value
}

// Sets a key as given, so its reading still applies the row's minimums. A key no row names
// sets nothing: nothing could read it.
func (self *proSidecarLimitValues) set(limitKey string, value int64) {
	if rowIndex, ok := limitKeyProSidecarRowIndexes[limitKey]; ok {
		self[rowIndex] = value
	}
}

// One tier's sidecar limits, and with them sidecar_abuse:'s, which are equal on every tier.
type ProSidecarLimits struct {
	values proSidecarLimitValues
}

// A key's limit. Zero is "not included", and an unknown key reads zero. The closure floor,
// allowance and pacing read their minimums, so an unusable value never stops erasure.
func (self *ProSidecarLimits) Limit(limitKey string) int64 {
	return self.values.limit(limitKey)
}

// Every key's limit, for QuotaLimits.Limits.
func (self *ProSidecarLimits) Limits() map[string]int64 {
	limitKeyValues := make(map[string]int64, len(proSidecarLimitRows))
	for rowIndex := range proSidecarLimitRows {
		limitKey := proSidecarLimitRows[rowIndex].key
		limitKeyValues[limitKey] = self.Limit(limitKey)
	}
	return limitKeyValues
}

// A copy with one key set, leaving self as it was. Whether a row may set the key is the
// caller's: Limit still applies the row's minimums either way.
func (self ProSidecarLimits) withLimit(limitKey string, value int64) ProSidecarLimits {
	self.values.set(limitKey, value)
	return self
}

// sidecar_abuse:, for a reader with no tier or network in hand. Each tier's ProSidecarLimits
// reads these keys too, so a reader with one uses that, and sees a network's §21 rows.
type ProSidecarAbuse struct {
	values proSidecarLimitValues
}

// A key's limit, as ProSidecarLimits.Limit. A tier's key reads zero here.
func (self *ProSidecarAbuse) Limit(limitKey string) int64 {
	return self.values.limit(limitKey)
}

// The team: block (DESIGN.md 20.4, 20.9). Team's tier itself is TierFor(NetworkTierTeam).
type ProTeamConfig struct {
	// The subscription in USD a month: the base, which covers IncludedSeats billable seats,
	// and each seat past them. Zero is no price, which no purchase path will sell.
	PriceBaseUsd float64
	PriceSeatUsd float64
	// Billable seats in the base. Auditor and Billing admin never count.
	IncludedSeats int64
	// Storage is metered above Sidecar's `storage` and refused above this.
	StorageOverageMax ByteCount
	// The team_credit balance that transfer overage draws, last.
	CreditLine ByteCount
	Sidecar    ProSidecarLimits

	// Read through the accessors, because zero here is a withheld overage and never $0.
	overageTransferTibUsd     float64
	overageStorageGibMonthUsd float64
}

// The USD price of a TiB of transfer overage. False withholds that overage: a missing
// price is never $0 (D-QUOTA-2).
func (self *ProTeamConfig) OverageTransferTibUsd() (float64, bool) {
	return self.overageTransferTibUsd, 0 < self.overageTransferTibUsd
}

// The USD price of a GiB-month of storage overage. False withholds that overage, so
// storage is refused past Sidecar's `storage`. It ships false until the owner prices it
// (owner decision 122).
func (self *ProTeamConfig) OverageStorageGibMonthUsd() (float64, bool) {
	return self.overageStorageGibMonthUsd, 0 < self.overageStorageGibMonthUsd
}

// A tier's limits and features. Team is pro's concurrent_clients, data, data_period and
// features with team's sidecar: (D-QUOTA-1), read live from Pro like PriceTiers, and it
// carries no price of Pro's: Team sells at ProTeamConfig's. An unknown tier answers free's.
// Tier(pro bool) stays for readers of the two older tiers.
func (self *ProConfig) TierFor(tier NetworkTier) *ProTier {
	switch tier {
	case NetworkTierPro:
		return &self.Pro
	case NetworkTierTeam:
		teamTier := self.Pro
		teamTier.Sidecar = self.Team.Sidecar
		teamTier.PriceMonthlyUsd = 0
		teamTier.PriceYearlyUsd = 0
		return &teamTier
	default:
		return &self.Free
	}
}

// ----- raw yaml shapes -----

type proSidecarTierYaml struct {
	Sidecar map[string]any `yaml:"sidecar"`
}

// {base, seat}, unlike proPriceYaml's {monthly, yearly}.
type proTeamPriceYaml struct {
	Base any `yaml:"base"`
	Seat any `yaml:"seat"`
}

type proTeamOverageYaml struct {
	TransferTib     any `yaml:"transfer_tib"`
	StorageGibMonth any `yaml:"storage_gib_month"`
}

type proTeamYaml struct {
	PriceUsd          proTeamPriceYaml   `yaml:"price_usd"`
	IncludedSeats     any                `yaml:"included_seats"`
	StorageOverageMax any                `yaml:"storage_overage_max"`
	CreditLine        any                `yaml:"credit_line"`
	OverageUsd        proTeamOverageYaml `yaml:"overage_usd"`
	Sidecar           map[string]any     `yaml:"sidecar"`
}

// Everything this file reads from pro.yml, decoded apart from proConfigYaml, whose fields
// are required and whose decode panics. Leaves are `any` so that a value of any shape
// reaches parseProSidecarValue, and a block of the wrong shape fails only itself, because
// yaml.v3 decodes on past a type error.
type proSidecarConfigYaml struct {
	EnforceSidecarLimits bool               `yaml:"enforce_sidecar_limits"`
	EvsUnpaidReadRate    any                `yaml:"evs_unpaid_read_rate"`
	Free                 proSidecarTierYaml `yaml:"free"`
	Pro                  proSidecarTierYaml `yaml:"pro"`
	Team                 proTeamYaml        `yaml:"team"`
	SidecarAbuse         map[string]any     `yaml:"sidecar_abuse"`
}

// A yaml value as its kind, or zero and false.
func parseProSidecarValue(kind proSidecarKind, yamlValue any) (int64, bool) {
	switch kind {
	case proSidecarKindBool:
		enabled, ok := yamlValue.(bool)
		if !ok {
			return 0, false
		}
		if enabled {
			return 1, true
		}
		return 0, true

	case proSidecarKindDuration:
		durationString, ok := yamlValue.(string)
		if !ok {
			return 0, false
		}
		duration, err := time.ParseDuration(durationString)
		if err != nil || duration <= 0 {
			return 0, false
		}
		return duration.Milliseconds(), true
	}

	switch value := yamlValue.(type) {
	case int:
		if 0 <= value {
			return int64(value), true
		}
	case int64:
		if 0 <= value {
			return value, true
		}
	case string:
		if value == proSidecarNoneYaml {
			return proSidecarLimitNone, true
		}
		// ParseByteCount goes through a float: a leading digit rules out nan and inf, and
		// what cannot fit, such as 1e30gib, comes back as one of int64's bounds
		if kind == proSidecarKindBytes && value != "" && '0' <= value[0] && value[0] <= '9' {
			byteCount, err := ParseByteCount(value)
			if err == nil && 0 <= byteCount && byteCount < math.MaxInt64 {
				return byteCount, true
			}
		}
	}
	return 0, false
}

// Fills one block's rows of values. A missing or unusable value logs and stays zero. A key
// that is no row of this block logs and is ignored, which is also what keeps an abuse key
// out of a tier's block (D-QUOTA-7).
func parseProSidecarBlock(
	blockName string,
	block proSidecarBlock,
	yamlKeyValues map[string]any,
	values *proSidecarLimitValues,
) {
	if yamlKeyValues == nil {
		glog.Errorf("[pro]pro.yml has no %s block. Its limits read zero.\n", blockName)
		return
	}
	for rowIndex := range proSidecarLimitRows {
		row := &proSidecarLimitRows[rowIndex]
		if row.block != block {
			continue
		}
		yamlValue, ok := yamlKeyValues[row.key]
		if !ok {
			glog.Errorf("[pro]pro.yml has no %s.%s. It reads zero.\n", blockName, row.key)
			continue
		}
		value, ok := parseProSidecarValue(row.kind, yamlValue)
		if !ok {
			glog.Errorf("[pro]pro.yml %s.%s is unusable (%v). It reads zero.\n", blockName, row.key, yamlValue)
			continue
		}
		values[rowIndex] = value
	}
	for limitKey := range yamlKeyValues {
		if row, ok := proSidecarLimitRowForKey(limitKey); !ok || row.block != block {
			glog.Errorf("[pro]pro.yml %s.%s is no limit of that block. Ignoring it.\n", blockName, limitKey)
		}
	}
}

// One of team:'s amounts, where `none` has no meaning.
func parseProTeamAmount(name string, kind proSidecarKind, yamlValue any) int64 {
	value, ok := parseProSidecarValue(kind, yamlValue)
	if !ok || value < 0 {
		glog.Errorf("[pro]pro.yml team.%s is unusable (%v). It reads zero.\n", name, yamlValue)
		return 0
	}
	return value
}

// One of team:'s prices. An unset price reads zero with no complaint, since
// `storage_gib_month: ~` ships that way, and zero is no price.
func parseProTeamPriceUsd(name string, yamlValue any) float64 {
	var usd float64
	switch value := yamlValue.(type) {
	case nil:
		return 0
	case int:
		usd = float64(value)
	case float64:
		usd = value
	default:
		usd = math.NaN()
	}
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		glog.Errorf("[pro]pro.yml team.%s is unusable (%v). It reads as no price.\n", name, yamlValue)
		return 0
	}
	return usd
}

// Fills c's sidecar fields from pro.yml's content. Nothing here panics on what the file
// holds: see this file's header.
func parseProSidecarConfig(proYml []byte, c *ProConfig) {
	var y proSidecarConfigYaml
	if err := yaml.Unmarshal(proYml, &y); err != nil {
		glog.Errorf("[pro]pro.yml has unusable sidecar blocks. Each reads zero. err = %s\n", err)
	}

	c.EnforceSidecarLimits = y.EnforceSidecarLimits

	evsUnpaidReadRate, ok := parseProSidecarValue(proSidecarKindBytes, y.EvsUnpaidReadRate)
	if !ok {
		glog.Errorf("[pro]pro.yml evs_unpaid_read_rate is unusable (%v). It reads zero.\n", y.EvsUnpaidReadRate)
	}
	c.EvsUnpaidReadRate = evsUnpaidReadRate

	var sharedValues proSidecarLimitValues
	parseProSidecarBlock("sidecar_abuse", proSidecarBlockAbuse, y.SidecarAbuse, &sharedValues)
	c.SidecarAbuse = ProSidecarAbuse{values: sharedValues}
	sharedValues.set("evs_unpaid_read_rate", evsUnpaidReadRate)

	parseTier := func(blockName string, yamlKeyValues map[string]any) ProSidecarLimits {
		tierLimits := ProSidecarLimits{values: sharedValues}
		parseProSidecarBlock(blockName, proSidecarBlockTier, yamlKeyValues, &tierLimits.values)
		return tierLimits
	}
	c.Free.Sidecar = parseTier("free.sidecar", y.Free.Sidecar)
	c.Pro.Sidecar = parseTier("pro.sidecar", y.Pro.Sidecar)
	c.Team = ProTeamConfig{
		PriceBaseUsd:              parseProTeamPriceUsd("price_usd.base", y.Team.PriceUsd.Base),
		PriceSeatUsd:              parseProTeamPriceUsd("price_usd.seat", y.Team.PriceUsd.Seat),
		IncludedSeats:             parseProTeamAmount("included_seats", proSidecarKindCount, y.Team.IncludedSeats),
		StorageOverageMax:         parseProTeamAmount("storage_overage_max", proSidecarKindBytes, y.Team.StorageOverageMax),
		CreditLine:                parseProTeamAmount("credit_line", proSidecarKindBytes, y.Team.CreditLine),
		Sidecar:                   parseTier("team.sidecar", y.Team.Sidecar),
		overageTransferTibUsd:     parseProTeamPriceUsd("overage_usd.transfer_tib", y.Team.OverageUsd.TransferTib),
		overageStorageGibMonthUsd: parseProTeamPriceUsd("overage_usd.storage_gib_month", y.Team.OverageUsd.StorageGibMonth),
	}
}

// Overrides the sidecar limit enforcement flag on the process's parsed config, returning a
// restore function. Same caveats as Testing_SetEnforceConcurrentClients. For tests that
// exercise a dark limit.
func Testing_SetEnforceSidecarLimits(enforce bool) func() {
	c := Pro()
	prev := c.EnforceSidecarLimits
	c.EnforceSidecarLimits = enforce
	return func() { c.EnforceSidecarLimits = prev }
}

// Overrides limits on the process's parsed config by key, returning a restore function.
// Same caveats as Testing_SetEnforceConcurrentClients. A tier key changes that tier, and
// any other key changes every tier, since those are equal on every tier. It works with no
// pro.yml, where a limit otherwise reads zero, and it panics on a key no row names, so a
// test cannot set a limit nothing reads.
func Testing_SetSidecarLimits(tier NetworkTier, limitKeyValues map[string]int64) func() {
	c := Pro()
	prevFree := c.Free.Sidecar
	prevPro := c.Pro.Sidecar
	prevTeam := c.Team.Sidecar
	prevAbuse := c.SidecarAbuse
	prevEvsUnpaidReadRate := c.EvsUnpaidReadRate

	tierLimits := &c.Free.Sidecar
	switch tier {
	case NetworkTierPro:
		tierLimits = &c.Pro.Sidecar
	case NetworkTierTeam:
		tierLimits = &c.Team.Sidecar
	}
	setEveryTier := func(limitKey string, value int64) {
		c.Free.Sidecar.values.set(limitKey, value)
		c.Pro.Sidecar.values.set(limitKey, value)
		c.Team.Sidecar.values.set(limitKey, value)
	}
	for limitKey, value := range limitKeyValues {
		row, ok := proSidecarLimitRowForKey(limitKey)
		if !ok {
			panic(fmt.Errorf("no sidecar limit is named %q", limitKey))
		}
		switch row.block {
		case proSidecarBlockTier:
			tierLimits.values.set(limitKey, value)
		case proSidecarBlockAbuse:
			c.SidecarAbuse.values.set(limitKey, value)
			setEveryTier(limitKey, value)
		case proSidecarBlockTop:
			c.EvsUnpaidReadRate = value
			setEveryTier(limitKey, value)
		}
	}

	return func() {
		c.Free.Sidecar = prevFree
		c.Pro.Sidecar = prevPro
		c.Team.Sidecar = prevTeam
		c.SidecarAbuse = prevAbuse
		c.EvsUnpaidReadRate = prevEvsUnpaidReadRate
	}
}
