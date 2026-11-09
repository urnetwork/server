package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// DESIGN.md 20.6's relayed bytes (owner decision 10; R-QUOTA-3, D-QUOTA-4, FS-45, X11): a Network-mode
// contract between two marked sidecar clients (strict_exp) of one network, riding no P2P stream, is
// paid by that network, reserved at CreateContract and debited from close reports
// (relayed_contract_settle.go). Today's pairs and sdk devices are untouched: only two marked clients
// of one network take this path.
//
//	when     the pair has no joinable stream at the read the contract's stream binding makes before
//	         the transaction (contract_stream_binding.go); a pair on a stream reserves nothing, since
//	         P2P bytes never count
//	reserve  before the transaction, from the network's balances in draw order (escrowDrawLess),
//	         through escrow's per-balance Redis reservations under the contract's own id: the first
//	         balance with room takes reserve-pending-share, whose grant is
//	         min(requested, max(MinContractTransferByteCount, available / 8)) with available read
//	         in the script, so grants shrink as the balance nears zero; later balances take the rest
//	         of that grant with reserve-pending. The reservation succeeding is the linearization
//	         point, and its pending marker the retry identity, as for escrow (withRedisContractAdmission)
//	sign     the grant replaces the requested size in the signed contract
//	refuse   nothing reserved, or less than the smallest contract: the network is marked spent and
//	         CreateContract answers InsufficientBalance; the refusal is no route rejection, and the
//	         contract degradation valve never opens here
//	custody  the contract row carries payer_network_id, set in its one INSERT
//	         (plan-QUOTA-relayed-custody): with no transfer_escrow row and positive bytes that shape
//	         names a relayed contract, since zero-escrow contracts set no payer
//	cost     a warm network reads its balances from a per-process draw-order cache, so the contract
//	         adds no Postgres round trip and one Redis script for a one-balance network; a cold one
//	         adds one read
//
// The credit each balance passes to the script is its cached room, the balance less its pending
// debits, at most DrawOrderCacheTtl old, so a debit applied within that time can over-admit by
// itself, as Redis loss over-admits (20.15). The rest of the network's availability is the cached
// room of the later balances, which leaves their reservations out: a network with several balances
// shrinks its grants by the first balance's exact availability and the others' estimate.

var ErrRelayedReservationUnavailable = errors.New("Relayed reservations need MG0 and MG2.")

// Settings of relayed reservations.
type RelayedReservationSettings struct {
	// Whether CreateContract reserves a relayed sidecar contract. plan-QUOTA-relayed-custody is
	// answered (QD-094), so the path is on wherever its tables are linked; off, such a contract is
	// created as any Network contract, unbilled.
	Enabled bool
	// How long one read of a network's draw-order balances answers before the next, so a warm
	// network's relayed contracts read nothing from Postgres. 20.6's cost row.
	DrawOrderCacheTtl time.Duration
	// The most networks the cache holds; past it the cache starts over.
	DrawOrderCacheMaxNetworkCount int
}

func DefaultRelayedReservationSettings() *RelayedReservationSettings {
	return &RelayedReservationSettings{
		Enabled:                       true,
		DrawOrderCacheTtl:             5 * time.Second,
		DrawOrderCacheMaxNetworkCount: 8192,
	}
}

var relayedReservationSettings = DefaultRelayedReservationSettings()

// Whether CreateContract reserves relayed sidecar contracts in this binary: the setting, and MG0's
// marks and MG2's journal and spent mark linked.
func RelayedReservationEnabled() bool {
	return relayedReservationSettings.Enabled && server.SidecarMg0Linked() && server.SidecarMg2Linked()
}

// Replaces the settings until the returned restore, with the draw-order cache emptied on both
// sides. Tests that call it must not run in parallel.
func Testing_SetRelayedReservationSettings(settings *RelayedReservationSettings) func() {
	previous := relayedReservationSettings
	relayedReservationSettings = settings
	relayedDrawOrderBalances.clear()
	return func() {
		relayedReservationSettings = previous
		relayedDrawOrderBalances.clear()
	}
}

// One network's balances in draw order, as read at readTime. Immutable once stored.
type relayedDrawOrderEntry struct {
	balances []*DrawOrderBalance
	readTime time.Time
}

// This process's draw-order balances by network, in front of LoadDrawOrderBalances. Safe for
// concurrent use.
type relayedDrawOrderCache struct {
	stateLock sync.Mutex
	entries   map[server.Id]*relayedDrawOrderEntry
	// test seam: counts each Postgres read the cache makes
	loads int
}

func newRelayedDrawOrderCache() *relayedDrawOrderCache {
	return &relayedDrawOrderCache{
		entries: map[server.Id]*relayedDrawOrderEntry{},
	}
}

// The network's balances in window at now, from the cache while its read is younger than the
// ttl, else from a new read on a pooled connection. Call it with no transaction open.
func (self *relayedDrawOrderCache) get(ctx context.Context, networkId server.Id, now time.Time) []*DrawOrderBalance {
	settings := relayedReservationSettings
	entry := func() *relayedDrawOrderEntry {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.entries[networkId]
	}()
	if entry != nil && now.Sub(entry.readTime) < settings.DrawOrderCacheTtl {
		// a balance whose window ended since the read draws nothing
		balances := []*DrawOrderBalance{}
		for _, balance := range entry.balances {
			if !balance.StartTime.After(now) && now.Before(balance.EndTime) {
				balances = append(balances, balance)
			}
		}
		return balances
	}
	balances := LoadDrawOrderBalances(ctx, networkId, now)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.loads++
		if settings.DrawOrderCacheMaxNetworkCount <= len(self.entries) {
			self.entries = map[server.Id]*relayedDrawOrderEntry{}
		}
		self.entries[networkId] = &relayedDrawOrderEntry{
			balances: balances,
			readTime: now,
		}
	}()
	return balances
}

// Drops the network's entry, so the next relayed contract reads its balances again.
func (self *relayedDrawOrderCache) forget(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.entries, networkId)
}

func (self *relayedDrawOrderCache) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.entries = map[server.Id]*relayedDrawOrderEntry{}
	self.loads = 0
}

var relayedDrawOrderBalances = newRelayedDrawOrderCache()

// The Postgres reads the draw-order cache has made since it was last cleared, for tests that count
// the warm path's round trips.
func Testing_RelayedDrawOrderCacheLoads() int {
	relayedDrawOrderBalances.stateLock.Lock()
	defer relayedDrawOrderBalances.stateLock.Unlock()
	return relayedDrawOrderBalances.loads
}

// Creates a Network-mode contract between two marked sidecar clients of one network, reserving its
// grant from the network's balances before the transaction (DESIGN.md 20.6's relayed bytes). It
// returns the contract, the grant to sign in place of the request, and the contract's deadline. A
// pair that rides a stream reserves nothing and is granted the request. A refusal marks the network
// spent and wraps the insufficient balance error, which CreateContract answers InsufficientBalance.
// The caller has clamped requested to [floor, MaxContractTransferByteCount].
func ReserveRelayedContract(
	ctx context.Context,
	networkId server.Id,
	sourceId server.Id,
	destinationId server.Id,
	requested ByteCount,
	floor ByteCount,
	usageOriginIsSource bool,
) (contractId server.Id, grant ByteCount, expirationTime time.Time, returnErr error) {
	if !RelayedReservationEnabled() {
		return server.Id{}, 0, time.Time{}, ErrRelayedReservationUnavailable
	}
	if requested <= 0 || floor <= 0 || requested < floor {
		return server.Id{}, 0, time.Time{}, fmt.Errorf("Relayed contract size %d is outside its floor %d.", requested, floor)
	}
	// Pin before acquisition so transaction retries reuse even an absent source.
	ctx = providerWorkSessionContext(ctx)
	// the pair's stream, read before any transaction (contract_stream_binding.go): a contract that
	// joins a stream rides it, and P2P bytes never count
	resolveContractStreamBinding(ctx)
	if binding := contractStreamBindingFromContext(ctx); binding != nil && func() bool {
		binding.stateLock.Lock()
		defer binding.stateLock.Unlock()
		return binding.streamId != nil
	}() {
		contractId, expirationTime, returnErr = createContractNoEscrowResolved(ctx, networkId, sourceId, networkId, destinationId, requested, usageOriginIsSource)
		return contractId, requested, expirationTime, returnErr
	}

	escrow, err := runRedisContractAdmission(ctx, func(ctx context.Context) (*TransferEscrow, error) {
		return createRelayedContract(ctx, networkId, sourceId, destinationId, requested, floor, usageOriginIsSource)
	})
	if err != nil {
		if errors.Is(err, errRedisReservationInsufficient) {
			// the next relayed contract reads the balances again
			relayedDrawOrderBalances.forget(networkId)
			if markErr := MarkNetworkSpent(ctx, networkId, server.NowUtc()); markErr != nil {
				glog.Errorf("[quota][relayed]%s spent mark not written: %s\n", networkId, markErr)
			}
		}
		return server.Id{}, 0, time.Time{}, err
	}
	leavePosts := server.EnterContractCreationStage(ctx, server.ContractStagePostCommit)
	notifyCommittedContractOrigin(ctx, sourceId, destinationId)
	leavePosts()
	func() {
		defer server.EnterContractCreationStage(ctx, server.ContractStageClientStamp)()
		StampTopLevelClientContractTime(ctx, sourceId)
	}()
	return escrow.ContractId, escrow.TransferByteCount, escrow.ExpirationTime, nil
}

// One attempt: reserve in draw order before the transaction, then publish in it. The returned
// escrow carries the reserved balances, which the admission acknowledges after commit.
func createRelayedContract(
	ctx context.Context,
	networkId server.Id,
	sourceId server.Id,
	destinationId server.Id,
	requested ByteCount,
	floor ByteCount,
	usageOriginIsSource bool,
) (*TransferEscrow, error) {
	admission := redisAdmissionFromContext(ctx)
	if admission == nil {
		return nil, errRedisPendingReservationMissing
	}
	reservation, finish := admission.startPendingReservation(networkId, requested)
	defer finish()
	selected, err := reserveRelayedBalances(ctx, admission, reservation, networkId, requested, floor)
	if err != nil {
		return nil, err
	}
	reservation.selected = selected
	var granted ByteCount
	for _, balance := range selected {
		granted += balance.BalanceByteCount
	}

	var escrow *TransferEscrow
	var returnErr error
	leaveTransaction := server.EnterContractCreationStage(ctx, server.ContractStageTransaction)
	server.Tx(ctx, func(tx server.PgTx) {
		escrow, returnErr = createRelayedContractInTx(ctx, tx, admission, reservation, networkId, sourceId, destinationId, granted, usageOriginIsSource)
	}, server.TxReadCommitted, server.OptNoRetry())
	leaveTransaction()
	return escrow, returnErr
}

// Reserves the grant from the network's balances in draw order, each reservation pending under the
// request's contract id. Answers the insufficient balance error when nothing, or less than the
// smallest contract, is reserved; the request's compensation releases what was reserved.
func reserveRelayedBalances(
	ctx context.Context,
	admission *redisContractAdmission,
	reservation *redisPendingReservation,
	networkId server.Id,
	requested ByteCount,
	floor ByteCount,
) ([]*TransferEscrowBalance, error) {
	defer server.EnterContractCreationStage(ctx, server.ContractStageGrantSelection)()
	balances := relayedDrawOrderBalances.get(ctx, networkId, server.NowUtc())
	first := -1
	for index, balance := range balances {
		if 0 < balance.RoomByteCount() {
			first = index
			break
		}
	}
	if first < 0 {
		return nil, fmt.Errorf("%w (0).", errRedisReservationInsufficient)
	}
	// the rest of the network's availability, read only as far as 20.6's 8 x the grant needs
	var others ByteCount
	for _, balance := range balances[first+1:] {
		if 8*requested <= others {
			break
		}
		others += balance.RoomByteCount()
	}

	selected := []*TransferEscrowBalance{}
	reserved := ByteCount(0)
	reservation.noteReservationRequest()
	admission.noteBalance(balances[first].BalanceId)
	amount, share, err := redisRelayedShareReservation(ctx, balances[first].BalanceId, admission.contractId,
		balances[first].RoomByteCount(), requested, floor, others)
	if err != nil {
		return nil, err
	}
	if amount == 0 {
		admission.noteZeroReservation(balances[first].BalanceId)
	} else {
		selected = append(selected, &TransferEscrowBalance{BalanceId: balances[first].BalanceId, BalanceByteCount: amount})
		reserved += amount
	}
	for _, balance := range balances[first+1:] {
		if share <= reserved {
			break
		}
		if balance.RoomByteCount() <= 0 {
			continue
		}
		admission.noteBalance(balance.BalanceId)
		amount, err := redisContractReservation(ctx, "reserve-pending", balance.BalanceId, admission.contractId,
			balance.RoomByteCount(), share-reserved, redisContractReservationLease, server.OptRedisKeepLateReplies())
		if err != nil {
			return nil, err
		}
		if amount == 0 {
			admission.noteZeroReservation(balance.BalanceId)
			continue
		}
		selected = append(selected, &TransferEscrowBalance{BalanceId: balance.BalanceId, BalanceByteCount: amount})
		reserved += amount
	}
	if reserved < floor {
		// below the smallest contract nothing fits a message: the balance is empty for a contract
		return nil, fmt.Errorf("%w (%d).", errRedisReservationInsufficient, reserved)
	}
	return selected, nil
}

// reserve-pending-share on one balance: reserves, pending under the contract's id, the least of
// requested, the balance's availability and max(floor, (that availability + others) / 8), and
// returns the amount and the share it aimed at. Outside any transaction.
func redisRelayedShareReservation(
	ctx context.Context,
	balanceId server.Id,
	contractId server.Id,
	credit ByteCount,
	requested ByteCount,
	floor ByteCount,
	others ByteCount,
) (amount ByteCount, share ByteCount, returnErr error) {
	bounded, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(bounded, func(r server.RedisClient) error {
		values, err := r.Eval(bounded, redisContractReservationScript, redisContractReservationKeys(balanceId),
			"reserve-pending-share", contractId.String(), strconv.FormatInt(credit, 10), strconv.FormatInt(requested, 10),
			redisContractReservationLease.Milliseconds(), strconv.FormatInt(floor, 10), strconv.FormatInt(max(0, others), 10)).Slice()
		var reply redis.Error
		if errors.As(err, &reply) && reply.Error() == "reservation is not pending" {
			return errRedisReservationNotPending
		}
		if err != nil {
			return err
		}
		if len(values) != 2 {
			return fmt.Errorf("invalid reservation response")
		}
		parse := func(value any) (ByteCount, error) {
			text, ok := value.(string)
			if !ok {
				return 0, fmt.Errorf("invalid reservation response")
			}
			return strconv.ParseInt(text, 10, 64)
		}
		amount, err = parse(values[0])
		if err != nil {
			return err
		}
		share, err = parse(values[1])
		if err != nil {
			return err
		}
		if amount < 0 || requested < amount || share < amount || requested < share {
			return fmt.Errorf("invalid reservation response")
		}
		return nil
	}, server.OptRedisNoPreflight(), server.OptRedisKeepLateReplies())
	result := "accepted"
	if returnErr != nil {
		result = "error"
	} else if amount == 0 {
		result = "refused"
	}
	redisContractReservationResults.WithLabelValues("reserve-pending-share", result).Inc()
	return
}

// Publishes the attempt's pending reservation as the contract, with SQL alone: the reservation
// preceded BEGIN, and its acknowledgement follows commit. The contract takes the admission's id,
// which keys its tokens and markers, and carries its payer in its one INSERT.
func createRelayedContractInTx(
	ctx context.Context,
	tx server.PgTx,
	admission *redisContractAdmission,
	reservation *redisPendingReservation,
	networkId server.Id,
	sourceId server.Id,
	destinationId server.Id,
	granted ByteCount,
	usageOriginIsSource bool,
) (*TransferEscrow, error) {
	// recovery of this request's tokens holds this transaction-scoped fence
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, redisContractAdmissionLock(admission.contractId)))
	if err := reservation.requirePublishable(); err != nil {
		return nil, err
	}
	var published bool
	server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1)`, admission.contractId).Scan(&published))
	if published {
		return nil, errors.New("Redis reservation request already has SQL custody; reconcile its original contract")
	}
	// a shard-owned probe pays from its private grant, as for any Network contract
	if _, err := validateProberShardPayerInTx(ctx, tx, networkId, networkId, server.Id{}); err != nil {
		return nil, err
	}
	if err := lockActiveContractClientsInTx(ctx, tx, networkId, sourceId, networkId, destinationId); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// From here the transaction owner decides commit or rollback; compensation never releases a
	// possibly committed contract (createRedisTransferEscrowInTx)
	admission.notePublication()
	streamId := bindContractStreamAtInsert(ctx, admission.contractId)
	var expirationTime time.Time
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		batch.Queue(
			`
				WITH creation_clock AS MATERIALIZED (
					SELECT clock_timestamp() AT TIME ZONE 'UTC' AS create_time
				)
				INSERT INTO transfer_contract (
					contract_id,
					source_network_id,
					source_id,
					destination_network_id,
					destination_id,
					transfer_byte_count,
					payer_network_id,
					usage_origin_is_source,
					create_time,
					expiration_time,
					stream_id
				)
				SELECT
					$1, $2, $3, $2, $4, $5, $2, $6,
					create_time, date_trunc('milliseconds', create_time) + $7 * INTERVAL '1 millisecond',
					$8
				FROM creation_clock
				RETURNING expiration_time
			`,
			admission.contractId,
			networkId,
			sourceId,
			destinationId,
			granted,
			usageOriginIsSource,
			DefaultContractExpiration.Milliseconds(),
			streamId,
		).QueryRow(func(row pgx.Row) error {
			return row.Scan(&expirationTime)
		})
		batch.Queue(contractExtenderInsertSql, admission.contractId, sourceId, destinationId, ContractPartySource, ContractPartyDestination)
	})
	server.AddTxCommitCount(tx, &contractOpenedCounter, 1)
	providerWorkRetainReservationInTx(ctx, tx, admission.contractId)
	contractHoleEventInTx(ctx, tx, admission.contractId, sourceId, destinationId, "create", expirationTime)
	return &TransferEscrow{
		ContractId:        admission.contractId,
		ExpirationTime:    expirationTime,
		TransferByteCount: granted,
		Priority:          TrustedPriority,
		Balances:          reservation.selected,
	}, nil
}
