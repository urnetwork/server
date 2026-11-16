// The bytes each extender relayed in an epoch the SN25 extender amendment
// covers (sn WHITEPAPER.md §8.6.2, owner 2026-11-16): half of the epoch's
// capped pool bytes Z' is split over the eligible extenders in proportion to
// them, the other half by unit (sn ledger SplitExtenderPool). They are the
// epoch component's relay rows, operator-attested like its census.
//
// An extender relayed a contract when its provider client is one of the
// contract's extender parties (contract_extender, connect/EXTENDER.md J2).
// Its relay bytes are its hop shares of such contracts that stay provider
// usage under the amendment (stContractSharesForEpoch: not a platform
// endpoint's, not on the origin's network), over the contracts the amended
// epoch reader counts -- a credited outcome, closed in the epoch's earning
// window -- that a balance funded and whose payer is no declared measurement
// or test network.
//
// The read is its own rather than a column of the amended usage read. That
// read takes every contract the window closes, about 2.4M an hour of a 7-day
// epoch, and a contract_extender probe for each would multiply its cost, while
// about one contract in two hundred has an extender party (11,360 party rows in
// an hour on Main on 2026-11-16). So this walks contract_extender by its create time
// index, in slices of a settings' span, each slice one short read-only
// statement joined to transfer_contract by its primary key (about 11k probes
// and 3 s an hour of contracts on Main on 2026-11-16, about 9 minutes for an
// epoch and its lookback), and keeps a contract only when its close falls in
// the earning window. A contract is created before it closes, so the walk
// starts the settings' lookback before the window opens, and a contract created
// earlier than that and closed in the window adds no relay bytes: of 1,957
// relayed parties sampled on Main on 2026-11-16, the longest a contract took to
// close was about 40 minutes. The relay rows are the operator's attestation,
// like the census; a contract whose outcome a dispute changes between this read
// and the usage read can differ between them by that contract. A contract is
// read once whatever its number of parties, and a provider client that is
// several of its parties counts once.
package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// How the relay read walks an epoch (GetStEpochExtenderRelay).
type StEpochRelaySettings struct {
	// Create-time span of one slice of contract_extender, read in one
	// statement. An hour holds about 11k parties (2026-11-16), a read of
	// about 3 s. Non-positive reads the whole walk in one statement.
	Span time.Duration
	// How long before the earning window opens the walk starts, which is the
	// longest after its creation a contract may close and still add relay
	// bytes: a day, against about 40 minutes at most in a sample of relayed
	// contracts on 2026-11-16. Non-positive starts at the window.
	Lookback time.Duration
	// Reads of one slice that may fail in a row, each from nothing, before
	// the read fails. Below 1 reads each slice once.
	Attempts int
	// Wait before a failed slice is read again.
	RetryDelay time.Duration
}

// Hour-long slices from a day before the window, each tried three times,
// 15 s apart.
func DefaultStEpochRelaySettings() *StEpochRelaySettings {
	return &StEpochRelaySettings{
		Span:       1 * time.Hour,
		Lookback:   24 * time.Hour,
		Attempts:   3,
		RetryDelay: 15 * time.Second,
	}
}

// The relay bytes of one epoch: each extender provider client's, and the
// contracts that added any.
type StEpochExtenderRelay struct {
	ClientIdRelayBytes map[server.Id]ByteCount
	Contracts          uint64
}

// The relay bytes of every client together.
func (self *StEpochExtenderRelay) Bytes() ByteCount {
	var total ByteCount
	for _, relayBytes := range self.ClientIdRelayBytes {
		total += relayBytes
	}
	return total
}

// The relay read's totals, added to one contract at a time. It is pure: the
// rows carry everything it reads.
type stEpochRelayReader struct {
	epoch     uint64
	amendment stExtenderAmendment
	// the payer networks the operator declares for measurement or testing
	measurementNetworkIds map[server.Id]bool
	relay                 StEpochExtenderRelay
}

// A reader of one epoch under the amendment, with nothing counted yet.
func newStEpochRelayReader(epoch uint64, amendment stExtenderAmendment, measurementNetworkIds map[server.Id]bool) *stEpochRelayReader {
	return &stEpochRelayReader{epoch: epoch, amendment: amendment, measurementNetworkIds: measurementNetworkIds,
		relay: StEpochExtenderRelay{ClientIdRelayBytes: map[server.Id]ByteCount{}}}
}

// Adds one contract of the earning window with the provider clients of its
// extender parties. Each party's share that stays provider usage is relay
// bytes of its client; a pooled share is not its client's, and an unfunded or
// measurement contract relays nothing the pool weighs. A contract with bytes
// needs its usage origin network and create time, as the amended usage read
// does.
func (self *stEpochRelayReader) add(row *stEpochUsageRow, snapshot *contractUsageSnapshot, partyClientIds []server.Id) error {
	if snapshot.ByteCount == 0 || row.payerNetworkId == nil || self.measurementNetworkIds[*row.payerNetworkId] {
		return nil
	}
	if row.originNetworkId == nil {
		return fmt.Errorf("subnet contract %s has usage shares but no usage origin", row.contractId)
	}
	if row.createTime == nil {
		return fmt.Errorf("subnet contract %s has usage shares but no create time", row.contractId)
	}
	shares, err := stContractSharesForEpoch(snapshot, self.epoch, *row.originNetworkId, *row.createTime, self.amendment)
	if err != nil {
		return fmt.Errorf("subnet contract %s: %w", row.contractId, err)
	}
	relayed := false
	for _, share := range shares.Providers {
		if share.ByteCount == 0 || !slices.Contains(partyClientIds, share.ClientId) {
			continue
		}
		total := self.relay.ClientIdRelayBytes[share.ClientId]
		if share.ByteCount < 0 || share.ByteCount > math.MaxInt64-total {
			return fmt.Errorf("subnet extender relay bytes overflow")
		}
		self.relay.ClientIdRelayBytes[share.ClientId] = total + share.ByteCount
		relayed = true
	}
	if relayed {
		self.relay.Contracts++
	}
	return nil
}

// Adds another reader's totals, one slice's, to this one.
func (self *stEpochRelayReader) merge(other *stEpochRelayReader) error {
	for clientId, relayBytes := range other.relay.ClientIdRelayBytes {
		total := self.relay.ClientIdRelayBytes[clientId]
		if relayBytes > math.MaxInt64-total {
			return fmt.Errorf("subnet extender relay bytes overflow")
		}
		self.relay.ClientIdRelayBytes[clientId] = total + relayBytes
	}
	self.relay.Contracts += other.relay.Contracts
	return nil
}

// The extender parties of the contracts created in [$1, $2) that closed with
// a credited outcome in [$3, $4), each with its contract's usage snapshot,
// usage origin network, payer and create time, ordered so a contract's parties
// are adjacent. Reads contract_extender_create_time_contract_id and the
// transfer_contract primary key.
const stEpochExtenderRelaySql = `
	SELECT
		contract_extender.contract_id,
		contract_extender.client_id,
		transfer_contract.provider_usage,
		transfer_contract.close_time,
		CASE WHEN transfer_contract.usage_origin_is_source THEN transfer_contract.source_network_id
			WHEN NOT transfer_contract.usage_origin_is_source THEN transfer_contract.destination_network_id END,
		transfer_contract.payer_network_id,
		transfer_contract.create_time
	FROM contract_extender
	INNER JOIN transfer_contract ON
		transfer_contract.contract_id = contract_extender.contract_id
	WHERE
		$1 <= contract_extender.create_time AND contract_extender.create_time < $2 AND
		$3 <= transfer_contract.close_time AND transfer_contract.close_time < $4 AND
		transfer_contract.outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
	ORDER BY contract_extender.contract_id, contract_extender.client_id
`

// Reads the relay bytes of the contracts created in [createStart, createEnd)
// that closed in [windowStart, windowEnd) into reader, which it resets first,
// in one read-only statement on a maintenance connection. A row the usage read
// would refuse is refused here too, as an stEpochUsageRefusal.
func readStEpochExtenderRelaySlice(
	ctx context.Context,
	reader *stEpochRelayReader,
	createStart time.Time,
	createEnd time.Time,
	windowStart time.Time,
	windowEnd time.Time,
) error {
	var returnErr error
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		// a rerun of the callback on a new connection reads from nothing
		reader.relay, returnErr = StEpochExtenderRelay{ClientIdRelayBytes: map[server.Id]ByteCount{}}, nil
		rows, err := conn.Query(ctx, stEpochExtenderRelaySql, createStart, createEnd, windowStart, windowEnd)
		if err != nil {
			returnErr = fmt.Errorf("read epoch extender relay: %w", err)
			return
		}
		defer rows.Close()
		// one contract's row and its parties, added once its last party is read
		var row *stEpochUsageRow
		partyClientIds := []server.Id{}
		flush := func() error {
			if row == nil {
				return nil
			}
			snapshot, err := checkStEpochUsageRow(row, reader.epoch)
			if err != nil {
				return &stEpochUsageRefusal{err: err}
			}
			if err := reader.add(row, snapshot, partyClientIds); err != nil {
				return &stEpochUsageRefusal{err: err}
			}
			return nil
		}
		for rows.Next() {
			next := &stEpochUsageRow{live: true}
			var clientId server.Id
			if err := rows.Scan(&next.contractId, &clientId, &next.data, &next.closedAt, &next.originNetworkId, &next.payerNetworkId, &next.createTime); err != nil {
				returnErr = err
				return
			}
			if row == nil || next.contractId != row.contractId {
				if err := flush(); err != nil {
					returnErr = err
					return
				}
				row, partyClientIds = next, []server.Id{}
			}
			if !slices.Contains(partyClientIds, clientId) {
				partyClientIds = append(partyClientIds, clientId)
			}
		}
		if returnErr = rows.Err(); returnErr != nil {
			return
		}
		returnErr = flush()
	}, server.OptReadOnly())
	return returnErr
}

// Called before each attempt of a slice of the relay read is read: an error
// fails that attempt. A seam the tests set; nil otherwise.
var stEpochRelaySliceHook func(sliceStart time.Time, attempt int) error

// Sets the relay read's seam for another package's test, and returns what
// restores it.
func Testing_SetStEpochRelaySliceHook(hook func(sliceStart time.Time, attempt int) error) (restore func()) {
	previous := stEpochRelaySliceHook
	stEpochRelaySliceHook = hook
	return func() { stEpochRelaySliceHook = previous }
}

// Reads the relay bytes of an epoch the amendment covers: its extender
// parties' unpooled shares of the funded, non-measurement contracts that
// closed with a credited outcome in the epoch's earning window, walking
// contract_extender by create time from settings' lookback before the window
// to its end, a slice of settings' span at a time. A slice counts only once it
// is read whole: one that fails is read again from nothing, up to settings'
// attempts, and a refused row fails the read at once. An epoch the amendment
// does not cover relays nothing the pool weighs, and reads nothing.
func GetStEpochExtenderRelay(
	ctx context.Context,
	epoch uint64,
	startTime time.Time,
	endTime time.Time,
	amendment StExtenderAmendment,
	measurementNetworkIds map[server.Id]bool,
	settings *StEpochRelaySettings,
) (*StEpochExtenderRelay, error) {
	reader := newStEpochRelayReader(epoch, amendment, measurementNetworkIds)
	if !amendment.InForce(epoch) {
		return &reader.relay, nil
	}
	if !startTime.Before(endTime) {
		return nil, fmt.Errorf("invalid subnet usage window")
	}
	transition, err := server.LoadProviderPayoutEarningPolicy(ctx)
	if err != nil {
		return nil, err
	}
	windowStart, windowEnd := transition.SnWindow(startTime, endTime)
	if !windowStart.Before(windowEnd) {
		return &reader.relay, nil
	}
	walkStart := windowStart.Add(-max(0, settings.Lookback))
	readStart := time.Now()
	sliceCount := 0
	for sliceStart := walkStart; sliceStart.Before(windowEnd); sliceCount++ {
		sliceEnd := windowEnd
		if 0 < settings.Span && sliceStart.Add(settings.Span).Before(windowEnd) {
			sliceEnd = sliceStart.Add(settings.Span)
		}
		for attempt := 1; ; attempt++ {
			// a slice's own reader, merged only once the slice is read whole
			sliceReader := newStEpochRelayReader(epoch, amendment, measurementNetworkIds)
			err := func() error {
				if hook := stEpochRelaySliceHook; hook != nil {
					if err := hook(sliceStart, attempt); err != nil {
						return err
					}
				}
				return readStEpochExtenderRelaySlice(ctx, sliceReader, sliceStart, sliceEnd, windowStart, windowEnd)
			}()
			if err == nil {
				if err := reader.merge(sliceReader); err != nil {
					return nil, err
				}
				break
			}
			var refusal *stEpochUsageRefusal
			if errors.As(err, &refusal) || ctx.Err() != nil || max(1, settings.Attempts) <= attempt {
				return nil, fmt.Errorf("epoch %d extender relay slice from %s: %w", epoch, sliceStart.UTC().Format(time.RFC3339), err)
			}
			glog.Infof("[st]epoch %d extender relay slice from %s failed on attempt %d, read again: %s\n", epoch, sliceStart.UTC().Format(time.RFC3339), attempt, err)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(settings.RetryDelay):
			}
		}
		sliceStart = sliceEnd
	}
	glog.Infof("[st]epoch %d extender relay read %d slices in %s: %d bytes relayed by %d extender clients in %d contracts\n",
		epoch, sliceCount, time.Since(readStart).Round(time.Millisecond), reader.relay.Bytes(), len(reader.relay.ClientIdRelayBytes), reader.relay.Contracts)
	return &reader.relay, nil
}
