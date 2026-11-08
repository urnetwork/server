package model

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// §20.4's tier keys, Community / Pro / Team, typed from the design and not from pro.yml.
// §14's smtp_burst_window is plan-SMTP-small-readings' item 6, and top_level_client_limit
// is plan-QUOTA-sidecar-clients-store's 100 on every tier.
var proSidecarDesignTierLimits = []struct {
	limitKey string
	free     int64
	pro      int64
	team     int64
}{
	{limitKey: "storage", free: 1 * Gib, pro: 20 * Gib, team: 200 * Gib},
	{limitKey: "sidecar_clients", free: 20, pro: 200, team: 2000},
	{limitKey: "top_level_client_limit", free: 100, pro: 100, team: 100},
	{limitKey: "dns_entries", free: 5, pro: 200, team: 2000},
	{limitKey: "tls_endpoints", free: 1, pro: 20, team: 200},
	{limitKey: "config_admissions", free: 50, pro: 500, team: 5000},
	{limitKey: "config_statements_per_min", free: 60, pro: 60, team: 60},
	{limitKey: "config_statements_burst", free: 600, pro: 600, team: 600},
	{limitKey: "config_statements_per_day", free: 2000, pro: 2000, team: 20000},
	{limitKey: "config_bases_per_min", free: 1, pro: 1, team: 1},
	{limitKey: "config_bases_per_day", free: 200, pro: 200, team: 200},
	{limitKey: "store_weight", free: 1, pro: 4, team: 16},
	{limitKey: "evs_drops_per_day", free: 20, pro: 100, team: 1000},
	{limitKey: "acl_rules", free: 50, pro: 500, team: 5000},
	{limitKey: "acl_groups", free: 0, pro: 1, team: 1},
	{limitKey: "acl_requests", free: 1, pro: 1, team: 1},
	{limitKey: "acl_approvers_two", free: 0, pro: 0, team: 1},
	{limitKey: "acl_network_shares", free: 0, pro: 1, team: 1},
	{limitKey: "smtp_connections_per_day", free: 5, pro: 200, team: 20000},
	{limitKey: "smtp_bytes_per_day", free: 5 * Mib, pro: 1 * Gib, team: 20 * Gib},
	{limitKey: "smtp_recipients_per_message", free: 5, pro: 50, team: 100},
	{limitKey: "smtp_recipients_per_day", free: 25, pro: 1000, team: 100000},
	{limitKey: "smtp_message_bytes", free: 5 * Mib, pro: 25 * Mib, team: 35 * Mib},
	{limitKey: "smtp_burst", free: 5, pro: 50, team: 2000},
	{
		limitKey: "smtp_burst_window",
		free:     (10 * time.Minute).Milliseconds(),
		pro:      (10 * time.Minute).Milliseconds(),
		team:     time.Hour.Milliseconds(),
	},
	{limitKey: "smtp_concurrent_connections", free: 1, pro: 4, team: 32},
	{limitKey: "mail_inbound_message_bytes", free: 25 * Mib, pro: 50 * Mib, team: 50 * Mib},
	{limitKey: "mail_burners_active", free: 10, pro: 500, team: -1},
	{limitKey: "mail_burner_creates_per_day", free: 3, pro: 50, team: -1},
	{limitKey: "tls_endpoint_concurrent", free: 256, pro: 2048, team: 8192},
	{limitKey: "tls_endpoint_new_per_s", free: 50, pro: 200, team: 1000},
	{limitKey: "tls_endpoint_mbit_per_s", free: 20, pro: 200, team: 1000},
}

// §20.4's sidecar_abuse: keys, and the caps it only cites (plan-QUOTA-small-readings' reading
// 1): §11.10, §11.12, §12.7, §13.9 and §14.7, with the keys later readings added for them:
// plan-EVS1-subscription-caps #3's receipt pair, plan-TLS-small-readings #10's aggregate,
// plan-MAIL-small-readings #16's repos per message, plan-SMTP-shared-counters #3's
// concurrency and plan-SMTP-small-readings #8's bounce pair. An area adding an abuse key need
// not add it here: TestProYmlBlocksMatchTheTable holds pro.yml to the table.
var proSidecarDesignAbuseLimits = []struct {
	limitKey string
	value    int64
}{
	{limitKey: "mail_group_members", value: 100},
	{limitKey: "mail_inbound_recipients", value: 100},
	{limitKey: "mail_inbound_repos_per_message", value: 8},
	{limitKey: "probation_dns_entries", value: 5},
	{limitKey: "evs_closure_bytes_per_day", value: 64 * Mib},
	{limitKey: "evs_closure_bytes_floor", value: 4 * Mib},
	{limitKey: "evs_closure_uploads_per_s", value: 4},
	{limitKey: "evs_drop_previews_per_day", value: 20},
	{limitKey: "evs_subscribers_per_repo", value: 1000},
	{limitKey: "evs_repos_per_client", value: 500},
	{limitKey: "evs_read_receipts_per_repo_day", value: 256},
	{limitKey: "evs_read_receipt_bytes", value: 4 * Kib},
	{limitKey: "dns_changes_per_hour", value: 60},
	{limitKey: "dns_challenges_per_name_hour", value: 10},
	{limitKey: "dns_dkim_selectors", value: 64},
	{limitKey: "tls_visitor_concurrent", value: 64},
	{limitKey: "tls_visitor_new_per_s", value: 20},
	{limitKey: "tls_visitor_aggregate_multiple", value: 4},
	{limitKey: "smtp_community_connections_per_hour", value: 500},
	{limitKey: "smtp_community_median_multiple", value: 3},
	{limitKey: "smtp_destination_connections_per_hour", value: 200},
	{limitKey: "smtp_destination_concurrency", value: 10},
	{limitKey: "smtp_hard_bounce_percent", value: 10},
	{limitKey: "smtp_hard_bounce_floor", value: 10},
	{limitKey: "directory_requests_pending", value: 20},
	{limitKey: "directory_requests_per_day", value: 50},
	{limitKey: "directory_requests_per_run", value: 5},
	{limitKey: "directory_target_pending", value: 100},
	{limitKey: "directory_card_bytes", value: 16 * Kib},
}

var proSidecarEveryTier = []NetworkTier{NetworkTierFree, NetworkTierPro, NetworkTierTeam}

// What a key reads when pro.yml gives it nothing usable.
func proSidecarUnusableReading(limitKey string) int64 {
	switch limitKey {
	case "evs_closure_bytes_floor", "evs_closure_bytes_per_day":
		return 1088 * Kib
	case "evs_closure_uploads_per_s":
		return 4
	default:
		return 0
	}
}

// Parses pro.yml's content into an otherwise empty config, as loadProConfig does.
func parseProSidecarYml(proYml string) *ProConfig {
	c := &ProConfig{}
	parseProSidecarConfig([]byte(proYml), c)
	return c
}

// Fails on any key that does not read as nothing usable, on every tier of c.
func assertProSidecarInert(t *testing.T, label string, c *ProConfig) {
	t.Helper()
	for _, tier := range proSidecarEveryTier {
		limits := &c.TierFor(tier).Sidecar
		for i := range proSidecarLimitRows {
			limitKey := proSidecarLimitRows[i].key
			if value := limits.Limit(limitKey); value != proSidecarUnusableReading(limitKey) {
				t.Errorf("%s: %s %s reads %d", label, tier, limitKey, value)
			}
		}
	}
}

func TestProSidecarTiersMatchDesign(t *testing.T) {
	skipWithoutProYml(t)
	c := Pro()

	tierLimitKeys := map[string]bool{}
	for _, l := range proSidecarDesignTierLimits {
		tierLimitKeys[l.limitKey] = true
		for _, tierValue := range []struct {
			tier  NetworkTier
			value int64
		}{
			{tier: NetworkTierFree, value: l.free},
			{tier: NetworkTierPro, value: l.pro},
			{tier: NetworkTierTeam, value: l.team},
		} {
			limits := &c.TierFor(tierValue.tier).Sidecar
			if value := limits.Limit(l.limitKey); value != tierValue.value {
				t.Errorf("%s %s reads %d, and §20.4 says %d", tierValue.tier, l.limitKey, value, tierValue.value)
			}
			if value := limits.Limits()[l.limitKey]; value != tierValue.value {
				t.Errorf("%s %s lists %d, and §20.4 says %d", tierValue.tier, l.limitKey, value, tierValue.value)
			}
		}
	}
	for i := range proSidecarLimitRows {
		row := &proSidecarLimitRows[i]
		if row.block == proSidecarBlockTier && !tierLimitKeys[row.key] {
			t.Errorf("%s is a tier key with no row in this test", row.key)
		}
	}

	for _, l := range proSidecarDesignAbuseLimits {
		if value := c.SidecarAbuse.Limit(l.limitKey); value != l.value {
			t.Errorf("sidecar_abuse %s reads %d, and the design says %d", l.limitKey, value, l.value)
		}
		for _, tier := range proSidecarEveryTier {
			if value := c.TierFor(tier).Sidecar.Limit(l.limitKey); value != l.value {
				t.Errorf("%s %s reads %d, and the design says %d", tier, l.limitKey, value, l.value)
			}
		}
	}

	connect.AssertEqual(t, false, c.EnforceSidecarLimits)
	connect.AssertEqual(t, 1*Mib, c.EvsUnpaidReadRate)
	for _, tier := range proSidecarEveryTier {
		connect.AssertEqual(t, 1*Mib, c.TierFor(tier).Sidecar.Limit("evs_unpaid_read_rate"))
	}

	connect.AssertEqual(t, 100.0, c.Team.PriceBaseUsd)
	connect.AssertEqual(t, 20.0, c.Team.PriceSeatUsd)
	connect.AssertEqual(t, int64(5), c.Team.IncludedSeats)
	connect.AssertEqual(t, 2*Tib, c.Team.StorageOverageMax)
	connect.AssertEqual(t, 10*Tib, c.Team.CreditLine)
	transferTibUsd, transferPriced := c.Team.OverageTransferTibUsd()
	connect.AssertEqual(t, 3.0, transferTibUsd)
	connect.AssertEqual(t, true, transferPriced)
	// owner decision 122: unset until priced, so withheld and never $0
	_, storagePriced := c.Team.OverageStorageGibMonthUsd()
	connect.AssertEqual(t, false, storagePriced)
}

// A key in pro.yml that no row names would silently read zero, and a row with no value in
// pro.yml would too. Either way the table and the file have drifted.
func TestProYmlBlocksMatchTheTable(t *testing.T) {
	skipWithoutProYml(t)
	resource, err := server.Config.SimpleResource("pro.yml")
	if err != nil {
		t.Fatal(err)
	}
	var y proSidecarConfigYaml
	if err := yaml.Unmarshal(resource.Bytes(), &y); err != nil {
		t.Fatal(err)
	}

	for _, b := range []struct {
		blockName     string
		block         proSidecarBlock
		yamlKeyValues map[string]any
	}{
		{blockName: "free.sidecar", block: proSidecarBlockTier, yamlKeyValues: y.Free.Sidecar},
		{blockName: "pro.sidecar", block: proSidecarBlockTier, yamlKeyValues: y.Pro.Sidecar},
		{blockName: "team.sidecar", block: proSidecarBlockTier, yamlKeyValues: y.Team.Sidecar},
		{blockName: "sidecar_abuse", block: proSidecarBlockAbuse, yamlKeyValues: y.SidecarAbuse},
	} {
		for limitKey, yamlValue := range b.yamlKeyValues {
			row, ok := proSidecarLimitRowForKey(limitKey)
			if !ok || row.block != b.block {
				t.Errorf("%s.%s is no row of that block", b.blockName, limitKey)
				continue
			}
			if _, ok := parseProSidecarValue(row.kind, yamlValue); !ok {
				t.Errorf("%s.%s is unusable: %v", b.blockName, limitKey, yamlValue)
			}
		}
		for i := range proSidecarLimitRows {
			row := &proSidecarLimitRows[i]
			if row.block != b.block {
				continue
			}
			if _, ok := b.yamlKeyValues[row.key]; !ok {
				t.Errorf("%s has no %s", b.blockName, row.key)
			}
		}
	}
}

// D-QUOTA-7: quotas and what derives from them grow with the tier. `none` is the most a key
// can read.
func TestProSidecarTierLimitsGrowWithTheTier(t *testing.T) {
	skipWithoutProYml(t)
	c := Pro()

	atMost := func(lower int64, upper int64) bool {
		return upper == proSidecarLimitNone || (lower != proSidecarLimitNone && lower <= upper)
	}
	for i := range proSidecarLimitRows {
		row := &proSidecarLimitRows[i]
		if row.block != proSidecarBlockTier {
			continue
		}
		freeValue := c.TierFor(NetworkTierFree).Sidecar.Limit(row.key)
		proValue := c.TierFor(NetworkTierPro).Sidecar.Limit(row.key)
		teamValue := c.TierFor(NetworkTierTeam).Sidecar.Limit(row.key)
		// a tier's burst sits on a window of its own, so the two read together
		if row.key == "smtp_burst_window" {
			continue
		}
		if !atMost(freeValue, proValue) || !atMost(proValue, teamValue) {
			t.Errorf("%s falls with the tier: %d / %d / %d", row.key, freeValue, proValue, teamValue)
		}
	}
}

// Both harnesses: an enforced limit is the owner's separate deploy, never this file's default.
func TestSidecarLimitEnforcementIsDark(t *testing.T) {
	connect.AssertEqual(t, false, Pro().EnforceSidecarLimits)
}

func TestProSidecarEnforceSwitchReadsTheFile(t *testing.T) {
	connect.AssertEqual(t, true, parseProSidecarYml("enforce_sidecar_limits: true").EnforceSidecarLimits)
	connect.AssertEqual(t, false, parseProSidecarYml("enforce_sidecar_limits: false").EnforceSidecarLimits)
	connect.AssertEqual(t, false, parseProSidecarYml("").EnforceSidecarLimits)
	// garbage stays dark: a switch that cannot be read refuses nothing
	connect.AssertEqual(t, false, parseProSidecarYml("enforce_sidecar_limits: maybe").EnforceSidecarLimits)
	connect.AssertEqual(t, false, parseProSidecarYml("enforce_sidecar_limits: [true]").EnforceSidecarLimits)
}

func TestProSidecarLimitForms(t *testing.T) {
	c := parseProSidecarYml(`
free:
  sidecar:
    storage: 1gib
    smtp_bytes_per_day: 4096
    smtp_message_bytes: 512b
    mail_inbound_message_bytes: none
    sidecar_clients: 20
    mail_burners_active: none
    mail_burner_creates_per_day: none
    acl_requests: true
    acl_groups: false
    smtp_burst_window: 10m
    dns_entries: ~
`)
	limits := &c.TierFor(NetworkTierFree).Sidecar

	// bytes are binary: 1gib, 7.4% above PLAN.md's GB (§20.13)
	connect.AssertEqual(t, int64(1024*1024*1024), limits.Limit("storage"))
	connect.AssertEqual(t, int64(4096), limits.Limit("smtp_bytes_per_day"))
	connect.AssertEqual(t, int64(512), limits.Limit("smtp_message_bytes"))
	// `none` reads -1 on counts, rates and bytes, as QuotaLimits defines it
	connect.AssertEqual(t, int64(-1), limits.Limit("mail_inbound_message_bytes"))
	connect.AssertEqual(t, int64(20), limits.Limit("sidecar_clients"))
	connect.AssertEqual(t, int64(-1), limits.Limit("mail_burners_active"))
	connect.AssertEqual(t, int64(-1), limits.Limit("mail_burner_creates_per_day"))
	// a bool reads 1 or 0
	connect.AssertEqual(t, int64(1), limits.Limit("acl_requests"))
	connect.AssertEqual(t, int64(0), limits.Limit("acl_groups"))
	connect.AssertEqual(t, int64(600000), limits.Limit("smtp_burst_window"))
	// `~` is a missing value: zero, "not included"
	connect.AssertEqual(t, int64(0), limits.Limit("dns_entries"))
}

func TestProSidecarLimitsListsEveryRow(t *testing.T) {
	c := parseProSidecarYml(`
evs_unpaid_read_rate: 1mib
free:
  sidecar:
    sidecar_clients: 20
sidecar_abuse:
  directory_requests_pending: 20
`)
	limits := &c.TierFor(NetworkTierFree).Sidecar
	limitKeyValues := limits.Limits()
	connect.AssertEqual(t, len(proSidecarLimitRows), len(limitKeyValues))
	for limitKey, value := range limitKeyValues {
		if value != limits.Limit(limitKey) {
			t.Errorf("%s lists %d and reads %d", limitKey, value, limits.Limit(limitKey))
		}
	}
	connect.AssertEqual(t, int64(20), limitKeyValues["sidecar_clients"])
	// §16.11's daemons and readers apply the directory keys, so they ride this map
	connect.AssertEqual(t, int64(20), limitKeyValues["directory_requests_pending"])
	// evs has no pro.yml and reads the unpaid rate through this map too
	connect.AssertEqual(t, 1*Mib, limitKeyValues["evs_unpaid_read_rate"])
}

// A limit this build does not know fails closed, even if something wrote it.
func TestProSidecarUnknownKeyReadsZero(t *testing.T) {
	limits := ProSidecarLimits{}
	connect.AssertEqual(t, int64(0), limits.Limit("no_such_limit"))
	limits = limits.withLimit("no_such_limit", 5)
	connect.AssertEqual(t, int64(0), limits.Limit("no_such_limit"))
	connect.AssertEqual(t, ProSidecarLimits{}, limits)
	_, listed := limits.Limits()["no_such_limit"]
	connect.AssertEqual(t, false, listed)

	abuse := ProSidecarAbuse{}
	connect.AssertEqual(t, int64(0), abuse.Limit("no_such_limit"))
	// a tier's key is no abuse limit
	c := parseProSidecarYml("free: {sidecar: {storage: 1gib}}")
	connect.AssertEqual(t, int64(0), c.SidecarAbuse.Limit("storage"))
}

// Values no kind can hold, by the kind that must refuse them.
var proSidecarKindUnusableYamls = map[proSidecarKind][]string{
	proSidecarKindCount: {
		"abc", "-5", "1.5", "1e3", "1gib", "true", "[1, 2]", "{a: 1}", "~", `""`, "2,000",
		"99999999999999999999",
	},
	proSidecarKindRate: {
		"abc", "-5", "1.5", "1e3", "1gib", "true", "[1, 2]", "{a: 1}", "~", `""`, "2,000",
		"99999999999999999999",
	},
	proSidecarKindBytes: {
		"abc", "-5", "1.5", "true", "[1, 2]", "{a: 1}", "~", `""`, "-1gib", "+1gib", "1gb", "1 gib",
		"nangib", "infgib", "-infgib", "1e30gib", "99999999999tib", "gib",
	},
	proSidecarKindBool: {
		"abc", "1", "0", "none", "[1, 2]", "{a: 1}", "~", `""`, "yes",
	},
	proSidecarKindDuration: {
		"abc", "-1h", "0s", "5", "none", "true", "[1, 2]", "{a: 1}", "~", `""`,
	},
}

func TestProSidecarUnusableValuesAreRefusedByKind(t *testing.T) {
	for kind, yamlForms := range proSidecarKindUnusableYamls {
		for _, yamlForm := range yamlForms {
			var yamlValue any
			if err := yaml.Unmarshal([]byte(yamlForm), &yamlValue); err != nil {
				t.Fatalf("%s: %s", yamlForm, err)
			}
			if value, ok := parseProSidecarValue(kind, yamlValue); ok || value != 0 {
				t.Errorf("kind %d took %s as %d", kind, yamlForm, value)
			}
		}
	}
}

// R-QUOTA-5: every key at once holding garbage neither panics nor enforces anything, and each
// reads as nothing usable.
func TestProSidecarGarbageValuesReadZero(t *testing.T) {
	formCount := 0
	for _, yamlForms := range proSidecarKindUnusableYamls {
		formCount = max(formCount, len(yamlForms))
	}

	for formIndex := range formCount {
		yamlForm := func(row *proSidecarLimitRow) string {
			yamlForms := proSidecarKindUnusableYamls[row.kind]
			return yamlForms[formIndex%len(yamlForms)]
		}
		var tierBlock strings.Builder
		var abuseBlock strings.Builder
		var topBlock strings.Builder
		for i := range proSidecarLimitRows {
			row := &proSidecarLimitRows[i]
			switch row.block {
			case proSidecarBlockTier:
				fmt.Fprintf(&tierBlock, "    %s: %s\n", row.key, yamlForm(row))
			case proSidecarBlockAbuse:
				fmt.Fprintf(&abuseBlock, "  %s: %s\n", row.key, yamlForm(row))
			case proSidecarBlockTop:
				fmt.Fprintf(&topBlock, "%s: %s\n", row.key, yamlForm(row))
			}
		}
		proYml := fmt.Sprintf(
			"%sfree:\n  sidecar:\n%spro:\n  sidecar:\n%steam:\n  sidecar:\n%ssidecar_abuse:\n%s",
			topBlock.String(), tierBlock.String(), tierBlock.String(), tierBlock.String(), abuseBlock.String(),
		)

		c := parseProSidecarYml(proYml)
		assertProSidecarInert(t, fmt.Sprintf("form %d", formIndex), c)
		connect.AssertEqual(t, false, c.EnforceSidecarLimits)
		connect.AssertEqual(t, ByteCount(0), c.EvsUnpaidReadRate)
	}
}

// A block of the wrong shape fails only itself, and reads as nothing usable.
func TestProSidecarGarbageBlocksReadZero(t *testing.T) {
	for _, proYml := range []string{
		"",
		"free: 5",
		"free: {sidecar: 5}",
		"free: {sidecar: [1, 2]}",
		"free: {sidecar: text}",
		"free: {sidecar: {storage: 1gib, storage: 2gib}}",
		"free: {sidecar: {1: 2}}",
		"pro: {sidecar: 5}",
		"team: 5",
		"team: text",
		"team: {sidecar: 5}",
		"team: {price_usd: 7}",
		"team: {price_usd: {monthly: 5, yearly: 40}}",
		"team: {overage_usd: 3}",
		"sidecar_abuse: 5",
		"sidecar_abuse: [1, 2]",
		"evs_unpaid_read_rate: {a: 1}",
		"free: [",
	} {
		c := parseProSidecarYml(proYml)
		assertProSidecarInert(t, proYml, c)
		connect.AssertEqual(t, false, c.EnforceSidecarLimits)
		connect.AssertEqual(t, 0.0, c.Team.PriceBaseUsd)
		connect.AssertEqual(t, 0.0, c.Team.PriceSeatUsd)
	}

	c := parseProSidecarYml(`
free: {sidecar: 5}
pro: {sidecar: {sidecar_clients: 200}}
team: {credit_line: 10tib, price_usd: 7}
sidecar_abuse: {directory_requests_pending: 20}
`)
	connect.AssertEqual(t, int64(0), c.TierFor(NetworkTierFree).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, int64(200), c.TierFor(NetworkTierPro).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, 10*Tib, c.Team.CreditLine)
	connect.AssertEqual(t, 0.0, c.Team.PriceBaseUsd)
	connect.AssertEqual(t, int64(20), c.TierFor(NetworkTierFree).Sidecar.Limit("directory_requests_pending"))
}

// The real load path: proConfigYaml's decode panics on a type error, so a sidecar value it
// never reads must not reach it. With no sidecar keys at all, a pro.yml from before them stays
// inert.
func TestProLoadSurvivesGarbageSidecarBlocks(t *testing.T) {
	legacyYml := `
free: {concurrent_clients: 2, data: 30gib, data_period: 24h}
pro: {concurrent_clients: 1000, data: 10tib, data_period: 720h}
referral: {bonus_per_referral: 3gib, period: 24h}
data_code: {duration: 8760h}
`
	for _, proYml := range []string{
		legacyYml,
		legacyYml + "enforce_sidecar_limits: maybe\nevs_unpaid_read_rate: [1]\nteam: 5\nsidecar_abuse: 5\n",
		`
free: {concurrent_clients: 2, data: 30gib, data_period: 24h, sidecar: 5}
pro: {concurrent_clients: 1000, data: 10tib, data_period: 720h, sidecar: {storage: garbage}}
team: {price_usd: {base: abc}, sidecar: [1]}
referral: {bonus_per_referral: 3gib, period: 24h}
data_code: {duration: 8760h}
`,
	} {
		func() {
			defer server.Config.PushSimpleResource("pro.yml", []byte(proYml))()
			c := loadProConfig()
			connect.AssertEqual(t, 2, c.MaxConcurrentClients(false))
			connect.AssertEqual(t, 1000, c.MaxConcurrentClients(true))
			connect.AssertEqual(t, false, c.EnforceSidecarLimits)
			assertProSidecarInert(t, proYml, c)
		}()
	}
}

// A present block must leave the required decode alone, and team:'s price is no pro.price_usd.
func TestProSidecarBlocksParse(t *testing.T) {
	proYml := `
enforce_sidecar_limits: true
evs_unpaid_read_rate: 1mib
free: {concurrent_clients: 2, data: 30gib, data_period: 24h, sidecar: {sidecar_clients: 20}}
pro: {concurrent_clients: 1000, data: 10tib, data_period: 720h, sidecar: {sidecar_clients: 200}}
team:
  price_usd: {base: 100, seat: 20}
  included_seats: 5
  storage_overage_max: 2tib
  credit_line: 10tib
  overage_usd: {transfer_tib: 3, storage_gib_month: ~}
  sidecar: {sidecar_clients: 2000}
referral: {bonus_per_referral: 3gib, period: 24h}
data_code: {duration: 8760h}
sidecar_abuse: {evs_closure_bytes_floor: 4mib}
`
	defer server.Config.PushSimpleResource("pro.yml", []byte(proYml))()
	c := loadProConfig()

	connect.AssertEqual(t, true, c.EnforceSidecarLimits)
	connect.AssertEqual(t, 1*Mib, c.EvsUnpaidReadRate)
	connect.AssertEqual(t, int64(20), c.TierFor(NetworkTierFree).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, int64(200), c.TierFor(NetworkTierPro).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, int64(2000), c.TierFor(NetworkTierTeam).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, 100.0, c.Team.PriceBaseUsd)
	connect.AssertEqual(t, 20.0, c.Team.PriceSeatUsd)
	connect.AssertEqual(t, int64(5), c.Team.IncludedSeats)
	connect.AssertEqual(t, 2*Tib, c.Team.StorageOverageMax)
	connect.AssertEqual(t, 10*Tib, c.Team.CreditLine)
	connect.AssertEqual(t, 4*Mib, c.SidecarAbuse.Limit("evs_closure_bytes_floor"))
	connect.AssertEqual(t, 4*Mib, c.TierFor(NetworkTierTeam).Sidecar.Limit("evs_closure_bytes_floor"))
}

// Owner decision 146: an unusable value, a low floor or a row cannot take erasure's allowance
// below one full re-key.
func TestProSidecarClosureFloor(t *testing.T) {
	for _, l := range []struct {
		yamlFloor     string
		floorValue    int64
		yamlAllowance string
		allowance     int64
	}{
		{yamlFloor: "4mib", floorValue: 4 * Mib, yamlAllowance: "64mib", allowance: 64 * Mib},
		// a floor at, under and just over 1,088 KiB
		{yamlFloor: "1088kib", floorValue: 1088 * Kib, yamlAllowance: "64mib", allowance: 64 * Mib},
		{yamlFloor: "1087kib", floorValue: 1088 * Kib, yamlAllowance: "64mib", allowance: 64 * Mib},
		{yamlFloor: "1089kib", floorValue: 1089 * Kib, yamlAllowance: "64mib", allowance: 64 * Mib},
		{yamlFloor: "1mib", floorValue: 1088 * Kib, yamlAllowance: "64mib", allowance: 64 * Mib},
		{yamlFloor: "0", floorValue: 1088 * Kib, yamlAllowance: "64mib", allowance: 64 * Mib},
		// an unusable floor, which no form of it escapes
		{yamlFloor: "garbage", floorValue: 1088 * Kib, yamlAllowance: "64mib", allowance: 64 * Mib},
		{yamlFloor: "none", floorValue: 1088 * Kib, yamlAllowance: "64mib", allowance: 64 * Mib},
		{yamlFloor: "~", floorValue: 1088 * Kib, yamlAllowance: "64mib", allowance: 64 * Mib},
		// an allowance under the floor, or unusable, reads the floor
		{yamlFloor: "4mib", floorValue: 4 * Mib, yamlAllowance: "1mib", allowance: 4 * Mib},
		{yamlFloor: "4mib", floorValue: 4 * Mib, yamlAllowance: "0", allowance: 4 * Mib},
		{yamlFloor: "4mib", floorValue: 4 * Mib, yamlAllowance: "garbage", allowance: 4 * Mib},
		{yamlFloor: "garbage", floorValue: 1088 * Kib, yamlAllowance: "garbage", allowance: 1088 * Kib},
		// none is no cap, which is above any floor
		{yamlFloor: "4mib", floorValue: 4 * Mib, yamlAllowance: "none", allowance: -1},
	} {
		c := parseProSidecarYml(fmt.Sprintf(
			"sidecar_abuse: {evs_closure_bytes_floor: %s, evs_closure_bytes_per_day: %s}",
			l.yamlFloor, l.yamlAllowance,
		))
		for _, tier := range proSidecarEveryTier {
			limits := &c.TierFor(tier).Sidecar
			if value := limits.Limit("evs_closure_bytes_floor"); value != l.floorValue {
				t.Errorf("%s floor %s reads %d", tier, l.yamlFloor, value)
			}
			if value := limits.Limit("evs_closure_bytes_per_day"); value != l.allowance {
				t.Errorf("%s allowance %s over floor %s reads %d", tier, l.yamlAllowance, l.yamlFloor, value)
			}
		}
		if value := c.SidecarAbuse.Limit("evs_closure_bytes_floor"); value != l.floorValue {
			t.Errorf("sidecar_abuse floor %s reads %d", l.yamlFloor, value)
		}
		if value := c.SidecarAbuse.Limit("evs_closure_bytes_per_day"); value != l.allowance {
			t.Errorf("sidecar_abuse allowance %s reads %d", l.yamlAllowance, value)
		}
	}

	// a value set after the file, as a network_sidecar_limit row sets one, reads no lower
	c := parseProSidecarYml("sidecar_abuse: {evs_closure_bytes_floor: 4mib, evs_closure_bytes_per_day: 64mib}")
	tightened := c.TierFor(NetworkTierFree).Sidecar.withLimit("evs_closure_bytes_per_day", 1)
	connect.AssertEqual(t, 4*Mib, tightened.Limit("evs_closure_bytes_per_day"))
	connect.AssertEqual(t, 4*Mib, tightened.Limits()["evs_closure_bytes_per_day"])
	connect.AssertEqual(t, 64*Mib, c.TierFor(NetworkTierFree).Sidecar.Limit("evs_closure_bytes_per_day"))
}

// §20.7's wait bounds are stated at 4 a second, and zero would stop erasure at the limit.
func TestProSidecarClosurePacing(t *testing.T) {
	for _, l := range []struct {
		yamlValue string
		value     int64
	}{
		{yamlValue: "4", value: 4},
		{yamlValue: "8", value: 8},
		{yamlValue: "1", value: 4},
		{yamlValue: "0", value: 4},
		{yamlValue: "none", value: 4},
		{yamlValue: "garbage", value: 4},
		{yamlValue: "~", value: 4},
	} {
		c := parseProSidecarYml(fmt.Sprintf("sidecar_abuse: {evs_closure_uploads_per_s: %s}", l.yamlValue))
		if value := c.SidecarAbuse.Limit("evs_closure_uploads_per_s"); value != l.value {
			t.Errorf("%s reads %d", l.yamlValue, value)
		}
		for _, tier := range proSidecarEveryTier {
			if value := c.TierFor(tier).Sidecar.Limit("evs_closure_uploads_per_s"); value != l.value {
				t.Errorf("%s %s reads %d", tier, l.yamlValue, value)
			}
		}
	}
}

// The floor and the pacing are the only keys a network_sidecar_limit row may not set, and
// the allowance is the one key held at the floor (owner decision 146).
func TestProSidecarOnlyClosureKeysAreFixed(t *testing.T) {
	for i := range proSidecarLimitRows {
		row := &proSidecarLimitRows[i]
		fixed := row.key == "evs_closure_bytes_floor" || row.key == "evs_closure_uploads_per_s"
		if row.fixed != fixed {
			t.Errorf("%s fixed is %t", row.key, row.fixed)
		}
		if (row.minValueKey != "") != (row.key == "evs_closure_bytes_per_day") {
			t.Errorf("%s minValueKey is %q", row.key, row.minValueKey)
		}
	}
}

// D-QUOTA-7: sidecar_abuse: is equal on every tier, and no tier's block can change it.
func TestProSidecarAbuseIsEqualOnEveryTier(t *testing.T) {
	c := parseProSidecarYml(`
free:
  sidecar:
    directory_requests_pending: 1
    evs_closure_bytes_floor: 8mib
    evs_closure_uploads_per_s: 8
    evs_unpaid_read_rate: 2mib
pro:
  sidecar:
    directory_requests_pending: 2
team:
  sidecar:
    directory_requests_pending: 3
sidecar_abuse:
  directory_requests_pending: 20
  evs_closure_bytes_floor: 4mib
  evs_closure_uploads_per_s: 4
`)
	for _, tier := range proSidecarEveryTier {
		limits := &c.TierFor(tier).Sidecar
		connect.AssertEqual(t, int64(20), limits.Limit("directory_requests_pending"))
		connect.AssertEqual(t, 4*Mib, limits.Limit("evs_closure_bytes_floor"))
		connect.AssertEqual(t, int64(4), limits.Limit("evs_closure_uploads_per_s"))
		connect.AssertEqual(t, ByteCount(0), limits.Limit("evs_unpaid_read_rate"))
	}

	// and the reverse: a quota in sidecar_abuse: sets nothing
	c = parseProSidecarYml("free: {sidecar: {storage: 1gib}}\nsidecar_abuse: {storage: 5gib}")
	connect.AssertEqual(t, 1*Gib, c.TierFor(NetworkTierFree).Sidecar.Limit("storage"))
}

// Owner decision 66, R-QUOTA-2: forwarding and sockets are unlimited, so no key may exist.
func TestProSidecarHasNoForwardingOrSocketKey(t *testing.T) {
	for i := range proSidecarLimitRows {
		limitKey := proSidecarLimitRows[i].key
		for _, word := range []string{"forward", "socket", "bridge", "port"} {
			if strings.Contains(limitKey, word) {
				t.Errorf("%s limits %s", limitKey, word)
			}
		}
	}
}

func TestProTierForTeamInheritsPro(t *testing.T) {
	c := parseProSidecarYml("team: {sidecar: {sidecar_clients: 2000}}\nfree: {sidecar: {sidecar_clients: 20}}")
	c.Pro.ConcurrentClients = 1000
	c.Pro.Data = 10 * Tib
	c.Pro.DataPeriod = 720 * time.Hour
	c.Pro.PriceMonthlyUsd = 5
	c.Pro.PriceYearlyUsd = 40
	c.Pro.HttpProxy = true
	c.Pro.HttpsProxy = true
	c.Pro.SocksProxy = true
	c.Pro.WireguardProxy = true

	tier := c.TierFor(NetworkTierTeam)
	connect.AssertEqual(t, 1000, tier.ConcurrentClients)
	connect.AssertEqual(t, 10*Tib, tier.Data)
	connect.AssertEqual(t, 720*time.Hour, tier.DataPeriod)
	connect.AssertEqual(t, true, tier.HttpProxy)
	connect.AssertEqual(t, true, tier.HttpsProxy)
	connect.AssertEqual(t, true, tier.SocksProxy)
	connect.AssertEqual(t, true, tier.WireguardProxy)
	connect.AssertEqual(t, int64(2000), tier.Sidecar.Limit("sidecar_clients"))
	// Team sells at its own prices, and a zero price is never sellable
	connect.AssertEqual(t, 0.0, tier.PriceMonthlyUsd)
	connect.AssertEqual(t, 0.0, tier.PriceYearlyUsd)

	// read live from Pro, and handing the tier out changes nothing of Pro's
	c.Pro.ConcurrentClients = 7
	connect.AssertEqual(t, 7, c.TierFor(NetworkTierTeam).ConcurrentClients)
	tier.ConcurrentClients = 9
	connect.AssertEqual(t, 7, c.Pro.ConcurrentClients)
	connect.AssertEqual(t, 5.0, c.Pro.PriceMonthlyUsd)
	connect.AssertEqual(t, int64(0), c.Pro.Sidecar.Limit("sidecar_clients"))
}

func TestProTierForOlderTiersAreTier(t *testing.T) {
	c := &ProConfig{}
	connect.AssertEqual(t, true, c.TierFor(NetworkTierFree) == c.Tier(false))
	connect.AssertEqual(t, true, c.TierFor(NetworkTierPro) == c.Tier(true))
}

func TestProTierForUnknownTierAnswersFree(t *testing.T) {
	c := parseProSidecarYml("free: {sidecar: {sidecar_clients: 20}}\npro: {sidecar: {sidecar_clients: 200}}")
	for _, tier := range []NetworkTier{"", "enterprise", "FREE", "Pro", "team "} {
		connect.AssertEqual(t, true, c.TierFor(tier) == &c.Free)
		connect.AssertEqual(t, int64(20), c.TierFor(tier).Sidecar.Limit("sidecar_clients"))
	}
}

// D-QUOTA-2: a missing price withholds that overage, and must never read as $0.
func TestProTeamOverageWithheldWithoutAPrice(t *testing.T) {
	for _, l := range []struct {
		yamlPrice string
		usd       float64
		priced    bool
	}{
		{yamlPrice: "3", usd: 3, priced: true},
		{yamlPrice: "0.25", usd: 0.25, priced: true},
		{yamlPrice: "~", usd: 0, priced: false},
		{yamlPrice: "0", usd: 0, priced: false},
		{yamlPrice: "0.0", usd: 0, priced: false},
		{yamlPrice: "-3", usd: 0, priced: false},
		{yamlPrice: "abc", usd: 0, priced: false},
		{yamlPrice: "none", usd: 0, priced: false},
		{yamlPrice: ".nan", usd: 0, priced: false},
		{yamlPrice: ".inf", usd: 0, priced: false},
		{yamlPrice: "[3]", usd: 0, priced: false},
	} {
		c := parseProSidecarYml(fmt.Sprintf(
			"team: {overage_usd: {transfer_tib: %s, storage_gib_month: %s}}",
			l.yamlPrice, l.yamlPrice,
		))
		for _, read := range []func() (float64, bool){
			c.Team.OverageTransferTibUsd,
			c.Team.OverageStorageGibMonthUsd,
		} {
			if usd, priced := read(); usd != l.usd || priced != l.priced {
				t.Errorf("%s reads %g, %t", l.yamlPrice, usd, priced)
			}
		}
	}

	// a key that is simply missing
	c := parseProSidecarYml("team: {overage_usd: {}}")
	_, priced := c.Team.OverageTransferTibUsd()
	connect.AssertEqual(t, false, priced)
	_, priced = c.Team.OverageStorageGibMonthUsd()
	connect.AssertEqual(t, false, priced)
	team := ProTeamConfig{}
	_, priced = team.OverageStorageGibMonthUsd()
	connect.AssertEqual(t, false, priced)
}

// None of team:'s amounts means anything as `none`, and a garbage one reads zero.
func TestProTeamAmountsReadZeroWhenUnusable(t *testing.T) {
	for _, yamlValue := range []string{"abc", "none", "~", "-5", "[1]", "{a: 1}", "1.5"} {
		c := parseProSidecarYml(fmt.Sprintf(
			"team: {included_seats: %s, storage_overage_max: %s, credit_line: %s}",
			yamlValue, yamlValue, yamlValue,
		))
		connect.AssertEqual(t, int64(0), c.Team.IncludedSeats)
		connect.AssertEqual(t, ByteCount(0), c.Team.StorageOverageMax)
		connect.AssertEqual(t, ByteCount(0), c.Team.CreditLine)
	}
}

func TestTestingSetEnforceSidecarLimits(t *testing.T) {
	c := Pro()
	restore := Testing_SetEnforceSidecarLimits(true)
	connect.AssertEqual(t, true, c.EnforceSidecarLimits)
	restore()
	connect.AssertEqual(t, false, c.EnforceSidecarLimits)
}

func TestTestingSetSidecarLimits(t *testing.T) {
	c := Pro()
	freeLimits := c.Free.Sidecar
	freeClients := freeLimits.Limit("sidecar_clients")
	proClients := c.TierFor(NetworkTierPro).Sidecar.Limit("sidecar_clients")
	teamClients := c.TierFor(NetworkTierTeam).Sidecar.Limit("sidecar_clients")
	pending := c.SidecarAbuse.Limit("directory_requests_pending")
	readRate := c.EvsUnpaidReadRate

	restoreTier := Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"sidecar_clients": 3})
	connect.AssertEqual(t, int64(3), c.TierFor(NetworkTierPro).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, freeClients, c.TierFor(NetworkTierFree).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, teamClients, c.TierFor(NetworkTierTeam).Sidecar.Limit("sidecar_clients"))

	restoreTeam := Testing_SetSidecarLimits(NetworkTierTeam, map[string]int64{"sidecar_clients": 4})
	connect.AssertEqual(t, int64(4), c.TierFor(NetworkTierTeam).Sidecar.Limit("sidecar_clients"))

	restoreFree := Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"sidecar_clients": 2})
	connect.AssertEqual(t, int64(2), c.TierFor(NetworkTierFree).Sidecar.Limit("sidecar_clients"))
	// a change is a copy: limits already handed out keep their values
	connect.AssertEqual(t, freeClients, freeLimits.Limit("sidecar_clients"))

	restoreShared := Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{
		"directory_requests_pending": 7,
		"evs_unpaid_read_rate":       9,
	})
	for _, tier := range proSidecarEveryTier {
		connect.AssertEqual(t, int64(7), c.TierFor(tier).Sidecar.Limit("directory_requests_pending"))
		connect.AssertEqual(t, int64(9), c.TierFor(tier).Sidecar.Limit("evs_unpaid_read_rate"))
	}
	connect.AssertEqual(t, int64(7), c.SidecarAbuse.Limit("directory_requests_pending"))
	connect.AssertEqual(t, ByteCount(9), c.EvsUnpaidReadRate)

	restoreShared()
	restoreFree()
	restoreTeam()
	restoreTier()
	connect.AssertEqual(t, freeClients, c.TierFor(NetworkTierFree).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, proClients, c.TierFor(NetworkTierPro).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, teamClients, c.TierFor(NetworkTierTeam).Sidecar.Limit("sidecar_clients"))
	connect.AssertEqual(t, pending, c.SidecarAbuse.Limit("directory_requests_pending"))
	connect.AssertEqual(t, readRate, c.EvsUnpaidReadRate)
}

func TestTestingSetSidecarLimitsRefusesAnUnknownKey(t *testing.T) {
	panicValue := func() (panicValue any) {
		defer func() { panicValue = recover() }()
		Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"no_such_limit": 1})()
		return nil
	}()
	connect.AssertEqual(t, true, panicValue != nil)
}
