package model

// The funding order: the order in which a payer's current grants fund its
// contracts, and in which a contract's escrows are charged when it settles.
//
// A payer's paid Pro grants come first. A paid Pro grant is a recurring Pro
// grant (grant kind pro) that carries subscription revenue (paid): the grant of
// a Pro cycle that a paid period funds (pro_grant_cycle_model.go), and, until it
// runs out, the calendar month's grant from before per-cycle grants. Every other
// grant follows: the daily free grant, referral grants, data packs and codes, an
// x402 Pro month (it records no revenue), the cycle grant of a period without
// revenue (a $0 trial invoice, a sandbox purchase, a manual upgrade), and the Pro
// balances that record no grant kind (the 600 GiB activation credit written
// before per-cycle grants, and a data code redeemed while data codes still
// conferred Pro, such as an AppSumo code redeemed in early 2026). A period's Pro
// marker holds no bytes, so it never funds. Within each group the grant that
// ends first funds first, then the one that started first, then the lower
// balance id, as before.
//
// Why: Pro networks get the daily free grant too (owner, 2026-11-11: "pro
// should get the 1tib per month plus the 30gb per day", the monthly amount
// later confirmed as pro.yml's 10 TiB). A daily grant ends within a day, before
// any cycle grant. In earliest-expiry order alone, a Pro network would spend its
// free data first every day. That traffic is unpaid and pays providers nothing.
// And while a network stayed under the daily amount, its cycle grant would fund
// no swept traffic, so the cycle's subscription revenue would never reach the
// provider subsidy pool: planSubsidyPayments counts a balance's subsidy revenue
// only through the swept traffic that balance funded. With paid Pro grants
// first, Pro traffic stays paid while the cycle grant has bytes. The daily grant
// funds a Pro network only once its cycle grant is used up or has expired, as in
// a payment grace.
//
// Only a payer holding a paid Pro grant changes order. Every network that got
// the daily grant before Pro networks did keeps its order: a free network with a
// data pack, or a network whose Pro comes from an old data code, still spends
// its daily grant first. A network holds an in-window pro balance exactly when
// it is Pro (pro_model.go), so a free network holds no paid Pro grant.

// The first group of the funding order, as a boolean over one transfer_balance
// row's columns, qualified by `alias` (empty for none). Never NULL: pro and paid
// are NOT NULL, and a NULL grant kind is distinct from pro.
func transferBalanceFundsFirstSqlOf(alias string) string {
	if alias != "" {
		alias += "."
	}
	return `(` + alias + `pro AND ` + alias + `paid AND ` + alias + `grant_kind IS NOT DISTINCT FROM '` + GrantKindPro + `')`
}

// transferBalanceFundsFirstSqlOf for a statement over transfer_balance alone.
const transferBalanceFundsFirstSql = `(pro AND paid AND grant_kind IS NOT DISTINCT FROM '` + GrantKindPro + `')`

// Orders grants by the funding order.
func compareTransferBalanceFunding(a *escrowTransferBalance, b *escrowTransferBalance) int {
	if a.fundsFirst != b.fundsFirst {
		if a.fundsFirst {
			return -1
		}
		return 1
	}
	if c := a.endTime.Compare(b.endTime); c != 0 {
		return c
	}
	if c := a.startTime.Compare(b.startTime); c != 0 {
		return c
	}
	return a.balanceId.Cmp(b.balanceId)
}

// The predicates of the two funding phases, appended to escrowTransferBalanceSql.
// Each phase is read in earliest-expiry order on the ordered grant index
// (transfer_balance_active_network_end_start_id). The first-group expression is
// never NULL, so every grant is in exactly one phase.
const (
	transferBalanceFundsFirstPhaseSql = ` AND ` + transferBalanceFundsFirstSql
	transferBalanceFundsLaterPhaseSql = ` AND NOT ` + transferBalanceFundsFirstSql
)

// The phases of the Redis admission census (selectRedisTransferBalances), from
// the payer's cached Pro entitlement. A payer that is not Pro holds no paid Pro
// grant, so it gets one earliest-expiry census with the same statements as
// before. A Pro payer gets its paid Pro grants, then the rest.
//
// The cache only chooses how to read. A payer cached as Pro that holds no paid
// Pro grant (an old data code's Pro, an x402 month, a trial, a payment grace)
// reads an empty first phase, one more statement, and keeps its order. A payer
// cached as not Pro just after it became Pro (writers refresh the cache on
// commit; another process's local entry lives ProLocalCacheTtl) is funded in
// earliest-expiry order until the entry turns over.
func transferBalanceFundingPhases(pro bool) []string {
	if !pro {
		return []string{""}
	}
	return []string{transferBalanceFundsFirstPhaseSql, transferBalanceFundsLaterPhaseSql}
}

// Whether the payer ($1) holds a current ($2) paid Pro grant with bytes left.
// It has the Pro entitlement read's shape (loadProNetworkWithConn), with no
// `active` term, so the partial grant index cannot serve it: it is one probe of
// transfer_balance_pro (network_id, pro, start_time, end_time). It names only
// parameters, so the statement evaluates it once, as a one-time gate.
const transferBalanceHoldsFundsFirstSql = `
	SELECT 1 FROM transfer_balance AS funds_first
	WHERE
		funds_first.network_id = $1 AND
		funds_first.pro = true AND
		funds_first.start_time <= $2 AND
		$2 < funds_first.end_time AND
		funds_first.paid AND
		funds_first.grant_kind = '` + GrantKindPro + `' AND
		0 < funds_first.balance_byte_count
`

// The first `limit` of the payer's ($1) current ($2) grants in the funding
// order, in one statement, for reads inside a contract transaction, which
// cannot consult the Pro cache. It reads no more grants than an
// earliest-expiry LIMIT read does. The paid Pro grants are read only when the
// gate finds one; the rest are read from the start of the ordered index, as
// before. The final sort orders at most twice `limit` rows. The columns are
// escrowTransferBalanceSql's.
func transferBalanceFundingPageSql(limit string) string {
	return `
	SELECT balance_id, paid, balance_byte_count, start_time, end_time, funds_first
	FROM (
		(` + escrowTransferBalanceSql + transferBalanceFundsFirstPhaseSql + ` AND EXISTS (` + transferBalanceHoldsFundsFirstSql + `)
			ORDER BY end_time, start_time, balance_id LIMIT ` + limit + `)
		UNION ALL
		(` + escrowTransferBalanceSql + transferBalanceFundsLaterPhaseSql + `
			ORDER BY end_time, start_time, balance_id LIMIT ` + limit + `)
	) AS funding
	ORDER BY funds_first DESC, end_time, start_time, balance_id
	LIMIT ` + limit
}
