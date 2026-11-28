package model

// The data SN25 counted for a network's provider clients (GET
// /network/provider-status, owner decision 2026-11-28, feature 14: show the
// data each device provided and the unpaid data provided, with figures that
// match what SN25 pays and no estimate shown as owed).
//
// The figures are the newest settled epoch's provider rows: the usage bytes
// settlement v2 split that epoch's operator pool by (sn
// protocol.AllocateLedgerEpoch), one row per provider client, with the
// coldkey the row was paid to. A row with a coldkey that the head tier did
// not exclude was paid in the pool; a head-excluded row's network earns
// through its own head seat instead; a row without a coldkey had no payout
// wallet, and SN25 paid nothing for it: that is the unpaid data. A client
// with no row provided nothing the epoch counted. They are data, never an
// amount: the amounts stay in GET /account/epochs.

import (
	"context"

	"github.com/urnetwork/server"
)

// How a client's provided data was paid in its epoch.
const (
	// counted in the epoch's operator pool, to the row's coldkey
	ProviderStatusProvidedPaid = "paid"
	// excluded from the pool for the network's head seat, which earns it
	ProviderStatusProvidedHead = "head"
	// no payout wallet: SN25 paid nothing for it
	ProviderStatusProvidedUnpaid = "unpaid"
	// no row: nothing the epoch counted
	ProviderStatusProvidedNone = "none"
)

// One client's data in the newest settled epoch.
type ProviderStatusProvided struct {
	Epoch uint64 `json:"epoch"`
	// the usage bytes of the client's row, 0 without one
	UsageByteCount ByteCount `json:"usage_byte_count"`
	// one of ProviderStatusProvided*
	Payout string `json:"payout"`
}

// The network's data in the newest settled epoch, over its top-level clients.
type ProviderStatusNetworkProvided struct {
	Epoch uint64 `json:"epoch"`
	// the usage bytes of the network's rows
	UsageByteCount ByteCount `json:"usage_byte_count"`
	// the usage bytes of the network's rows that had no payout wallet
	UnpaidByteCount ByteCount `json:"unpaid_byte_count"`
	// the network has more clients than the totals read, so they are a lower
	// bound
	Truncated bool `json:"truncated,omitempty"`
}

// A provider row as the status reads it: its usage, and whether it had a
// coldkey and was head-excluded.
type ProviderStatusProvidedRow struct {
	UsageByteCount ByteCount
	Coldkey        bool
	HeadExcluded   bool
}

// The payout of a row, by the rule the epoch's allocation applies.
func (self ProviderStatusProvidedRow) Payout() string {
	switch {
	case self.HeadExcluded:
		return ProviderStatusProvidedHead
	case self.Coldkey:
		return ProviderStatusProvidedPaid
	default:
		return ProviderStatusProvidedUnpaid
	}
}

// One client's data in epoch: its row, or none without one.
func NewProviderStatusProvided(epoch uint64, row *ProviderStatusProvidedRow) *ProviderStatusProvided {
	if row == nil {
		return &ProviderStatusProvided{
			Epoch:  epoch,
			Payout: ProviderStatusProvidedNone,
		}
	}
	return &ProviderStatusProvided{
		Epoch:          epoch,
		UsageByteCount: row.UsageByteCount,
		Payout:         row.Payout(),
	}
}

// The network's data in epoch from the rows of its clients.
func NewProviderStatusNetworkProvided(epoch uint64, rows []*ProviderStatusProvidedRow, truncated bool) *ProviderStatusNetworkProvided {
	provided := &ProviderStatusNetworkProvided{
		Epoch:     epoch,
		Truncated: truncated,
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		provided.UsageByteCount += row.UsageByteCount
		if row.Payout() == ProviderStatusProvidedUnpaid {
			provided.UnpaidByteCount += row.UsageByteCount
		}
	}
	return provided
}

// The most clients the network totals read (ProviderStatusNetworkProvided). It
// bounds the read to one index range of that many rows.
const ProviderStatusProvidedMaxClients = 10000

// The network's top-level clients, active or not, so a device removed since
// the epoch still counts toward the totals: at most limit, and whether there
// were more. One range of the network_client (network_id, active, client_id)
// index.
func ListProviderStatusProvidedClientIds(ctx context.Context, networkId server.Id, limit int) (clientIds []server.Id, truncated bool) {
	clientIds = []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		clientIds = []server.Id{}
		result, err := conn.Query(
			ctx,
			`
			SELECT client_id
			FROM network_client
			WHERE network_id = $1 AND source_client_id IS NULL
			LIMIT $2
			`,
			networkId,
			limit+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	if limit < len(clientIds) {
		return clientIds[:limit], true
	}
	return clientIds, false
}
