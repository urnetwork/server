// Extender eligibility (sn WHITEPAPER.md §8.6.2, E1 and E3–E5): one
// implementation for both censuses that judge an operator's extenders, the
// extender pool's (st_extender_census.go) and capture the flag's, whose X1 and
// X4–X6 are these rules (sn CAPTURETHEFLAG.md §4.2). Both read the same
// records and the same rules, so an extender is never eligible in one pool and
// excluded from the other in the same epoch for a rule the two share.
//
//   - E1, active: at the census the record is active and unrevoked, and one of
//     its addresses is active. Only then has it a unit: the keyed hash of its
//     active IPv4 address's /29, else of its active IPv6 address's /48.
//   - E3, distinct: active records that share a provider client, an IPv4
//     address or an IPv6 /64, directly or through other records, count once,
//     for the record with the lowest (client id, extender id) pair. The others
//     are duplicates. Every active record takes part, whatever else it fails,
//     so a record that cannot be paid still holds its client and addresses: a
//     host earns from at most one record, and a second client on the address of
//     a fleet-bound or operator-owned one earns nothing in this pool.
//   - E4, payable: the provider client's payout coldkey under the provider
//     rows' wallet selection (§8.2), and no active fleet binding at either of
//     the epoch's boundaries.
//   - E5, independent: not in the gated tier or on a durable network (gated),
//     not on its own canary channel (canary), and not on another network the
//     operator controls: one it declares, or one that hosts a canary extender
//     (declared).
//
// A capture the flag member is a provider client, judged by one record
// (Judged): its active record with the lowest extender id, else its lowest.
package model

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/urfoundation/sn/ledger"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// ExtenderEligibilityRules are the operator's facts eligibility needs beside
// the records: the unit key, the operator's own networks, and each provider
// client's payout coldkey and head binding.
type ExtenderEligibilityRules struct {
	// Unit is the keyed /29 or /48 prefix hash of an address, §8.4's head
	// unit under the same key and granularity
	// (VerifyEgressIpHashWithSettings).
	Unit func(netip.Addr) [32]byte
	// Durable are the operator's gated, durable networks
	// (extender.yml directory.durable_network_ids), Declared the other
	// networks it declares it controls, and CanaryNetworks the networks that
	// host one of its canary extenders (E5).
	Durable        map[server.Id]bool
	Declared       map[server.Id]bool
	CanaryNetworks map[server.Id]bool
	// Coldkeys are the provider clients' payout coldkeys under the wallet
	// selection provider rows use, absent or zero without one (E4).
	Coldkeys map[server.Id][32]byte
	// HeadBound are the provider clients with an active fleet binding at
	// either of the epoch's boundaries (E4).
	HeadBound map[server.Id]bool
}

// StExtenderCensusRules are the extender pool census's rules: the shared ones.
type StExtenderCensusRules = ExtenderEligibilityRules

// ExtenderEligibility is one record judged under E1 and E3–E5, with the flags
// of its census row (ledger.CensusFlags).
type ExtenderEligibility struct {
	Record  *StExtenderCensusRecord
	Flags   ledger.CensusFlags
	Unit    [32]byte
	Family  uint8
	Coldkey [32]byte
}

// Active is E1.
func (self *ExtenderEligibility) Active() bool {
	return self.Flags&ledger.CensusActive != 0
}

// Duplicate is E3: the record is active and another record of its component
// counts.
func (self *ExtenderEligibility) Duplicate() bool {
	return self.Flags&ledger.CensusDuplicate != 0
}

// OwnNetwork is E5's failure: the record is gated, a canary, or on another
// network the operator controls.
func (self *ExtenderEligibility) OwnNetwork() bool {
	return self.Flags&(ledger.CensusGated|ledger.CensusCanary|ledger.CensusDeclared) != 0
}

// HeadBound is E4's second half: the provider client has an active fleet
// binding at either boundary.
func (self *ExtenderEligibility) HeadBound() bool {
	return self.Flags&ledger.CensusHeadBound != 0
}

// Payable is E4's first half: the provider client has a payout coldkey.
func (self *ExtenderEligibility) Payable() bool {
	return self.Coldkey != ([32]byte{})
}

// Eligible is every rule at once, as ledger.CensusRow.Eligible reads a row.
func (self *ExtenderEligibility) Eligible() bool {
	return self.Flags == ledger.CensusActive && self.Payable()
}

// CensusRow is the record's census row.
func (self *ExtenderEligibility) CensusRow() ledger.CensusRow {
	return ledger.CensusRow{
		ExtenderID: [16]byte(self.Record.ExtenderId),
		ClientID:   [16]byte(self.Record.ClientId),
		Coldkey:    self.Coldkey,
		Unit:       self.Unit,
		Family:     self.Family,
		Flags:      self.Flags,
	}
}

// ExtenderEligibilities are an epoch's judged records, sorted by client id and
// extender id.
type ExtenderEligibilities []*ExtenderEligibility

// Judged is the record a provider client is judged by when it is judged as
// one extender (sn CAPTURETHEFLAG.md X1): its active record with the lowest
// extender id, else its lowest record; nil for a client with no record.
func (self ExtenderEligibilities) Judged(clientId server.Id) *ExtenderEligibility {
	i, _ := slices.BinarySearchFunc(self, clientId, func(e *ExtenderEligibility, clientId server.Id) int {
		return e.Record.ClientId.Cmp(clientId)
	})
	var lowest *ExtenderEligibility
	for ; i < len(self) && self[i].Record.ClientId == clientId; i++ {
		if self[i].Active() {
			return self[i]
		}
		if lowest == nil {
			lowest = self[i]
		}
	}
	return lowest
}

// CensusRows are the judged records' census rows, in their order.
func (self ExtenderEligibilities) CensusRows() []ledger.CensusRow {
	rows := make([]ledger.CensusRow, len(self))
	for i, eligibility := range self {
		rows[i] = eligibility.CensusRow()
	}
	return rows
}

// The address a record's unit is keyed from: its active IPv4 address, else
// its active IPv6 address, as a provider's head score pins its IPv4 hash
// first.
func extenderUnitAddress(record *StExtenderCensusRecord) (netip.Addr, uint8, bool) {
	for _, family := range []uint8{ledger.UnitIPv4, ledger.UnitIPv6} {
		for _, address := range record.Addresses {
			if address.Active && address.Ip.IsValid() && (address.Ip.Is4() == (family == ledger.UnitIPv4)) {
				return address.Ip, family, true
			}
		}
	}
	return netip.Addr{}, ledger.UnitNone, false
}

// The keys E3 joins records by: the provider client, each active IPv4 address
// and each active IPv6 address's /64.
func extenderDedupKeys(record *StExtenderCensusRecord) []string {
	keys := []string{"client:" + record.ClientId.String()}
	for _, address := range record.Addresses {
		switch {
		case !address.Active || !address.Ip.IsValid():
		case address.Ip.Is4():
			keys = append(keys, "ipv4:"+address.Ip.String())
		default:
			keys = append(keys, "ipv6:"+netip.PrefixFrom(address.Ip, 64).Masked().String())
		}
	}
	return keys
}

// DecideExtenderEligibility judges an epoch's records under E1 and E3–E5,
// sorted by client id and extender id. A record listed twice, or an active
// one whose unit key is zero, is an error.
func DecideExtenderEligibility(records []*StExtenderCensusRecord, rules *ExtenderEligibilityRules) (ExtenderEligibilities, error) {
	if rules == nil || rules.Unit == nil {
		return nil, errors.New("extender eligibility: no unit key")
	}
	records = slices.Clone(records)
	slices.SortFunc(records, func(a, b *StExtenderCensusRecord) int {
		if c := a.ClientId.Cmp(b.ClientId); c != 0 {
			return c
		}
		return a.ExtenderId.Cmp(b.ExtenderId)
	})
	judged := make(ExtenderEligibilities, len(records))
	listed := make(map[server.Id]bool, len(records))
	for i, record := range records {
		if listed[record.ExtenderId] {
			return nil, fmt.Errorf("extender eligibility: extender %s is listed twice", record.ExtenderId)
		}
		listed[record.ExtenderId] = true
		eligibility := &ExtenderEligibility{Record: record}
		// E1
		if address, family, ok := extenderUnitAddress(record); ok && record.Active && !record.Revoked {
			eligibility.Flags |= ledger.CensusActive
			eligibility.Unit, eligibility.Family = rules.Unit(address), family
			if eligibility.Unit == ([32]byte{}) {
				return nil, fmt.Errorf("extender eligibility: extender %s has a zero unit", record.ExtenderId)
			}
		}
		if record.Revoked {
			eligibility.Flags |= ledger.CensusRevoked
		}
		// E5
		if record.DirectoryTier != connect.ExtenderDirectoryTierOpen || rules.Durable[record.NetworkId] {
			eligibility.Flags |= ledger.CensusGated
		}
		if record.Canary {
			eligibility.Flags |= ledger.CensusCanary
		}
		if rules.Declared[record.NetworkId] || rules.CanaryNetworks[record.NetworkId] {
			eligibility.Flags |= ledger.CensusDeclared
		}
		// E4
		if rules.HeadBound[record.ClientId] {
			eligibility.Flags |= ledger.CensusHeadBound
		}
		eligibility.Coldkey = rules.Coldkeys[record.ClientId]
		judged[i] = eligibility
	}
	// E3 over the active records: a union of every key's records, each
	// component counted once, for its lowest (client id, extender id), which
	// in this order is its lowest index.
	parent := make([]int, len(judged))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	first := map[string]int{}
	for i, eligibility := range judged {
		if !eligibility.Active() {
			continue
		}
		for _, key := range extenderDedupKeys(eligibility.Record) {
			j, ok := first[key]
			if !ok {
				first[key] = i
				continue
			}
			if a, b := find(i), find(j); a != b {
				parent[max(a, b)] = min(a, b)
			}
		}
	}
	for i, eligibility := range judged {
		if eligibility.Active() && find(i) != i {
			eligibility.Flags |= ledger.CensusDuplicate
		}
	}
	return judged, nil
}

// GetExtenderCanaryNetworkIds reads the networks of every extender published
// on its own canary channel, which are the operator's own (E5).
func GetExtenderCanaryNetworkIds(ctx context.Context) []server.Id {
	networkIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT DISTINCT network_id FROM network_extender WHERE canary_channel IS NOT NULL ORDER BY network_id`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	return networkIds
}
