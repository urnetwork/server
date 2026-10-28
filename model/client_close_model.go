// Client close (CLIENTCLOSE1.md). Two per-client Redis keys whose keyspace
// events reach the connect and proxy processes, modeled on the stream hop set
// (`clientStreamHopsKey`):
//   - `{<clientId>}c_removed` is set after an api removal commits. Each connect
//     process closes the client's transports and resident, and each proxy
//     process closes the client's devices.
//   - `{<clientId>}c_close` holds close requests for the client: contracts
//     whose other party sent its final close, and peers removed through the
//     api. The process that hosts the client's resident takes them and sends
//     the client one `CloseSequences`.
//
// Both are best effort. A lost write or event leaves the contracts to the
// expiry sweep and the transports to the authorization recheck, as before.
package model

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// long enough for a process that resubscribes to find the flag of a client it
// still holds
const ClientRemovedTtl = 2 * time.Hour

// Renewed by each add. A client that does not connect in this time never
// takes its requests, and its contracts close at their deadline.
const ClientCloseTtl = 15 * time.Minute

// the most requests one `CloseSequences` carries
const ClientCloseTakeLimit = 1024

// the most open contracts, per direction, whose counterparties a removal asks
// to close
const clientRemovedOpenContractLimit = 4096

// Removals waiting for their open contracts to be read. A removal that finds
// the queue full is left to the sweep.
const clientRemovedQueueSize = 4096
const clientRemovedWorkerCount = 4
const clientRemovedReadTimeout = 30 * time.Second

const clientRemovedKeySuffix = "c_removed"
const clientCloseKeySuffix = "c_close"

const clientCloseContractPrefix = "c:"
const clientClosePeerPrefix = "p:"

func clientRemovedKey(clientId server.Id) string {
	return fmt.Sprintf("{%s}%s", clientId, clientRemovedKeySuffix)
}

func clientCloseKey(clientId server.Id) string {
	return fmt.Sprintf("{%s}%s", clientId, clientCloseKeySuffix)
}

// The psubscribe pattern for the keyspace channels of every client's removed
// flag on a db.
func ClientRemovedKeyEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:{*}%s", db, clientRemovedKeySuffix)
}

// The client id of a keyspace channel matching `ClientRemovedKeyEventPattern`.
func ParseClientRemovedKeyEvent(channel string) (server.Id, bool) {
	return parseClientKeyEvent(channel, clientRemovedKeySuffix)
}

// The psubscribe pattern for the keyspace channels of every client's close
// requests on a db.
func ClientCloseKeyEventPattern(db int) string {
	return fmt.Sprintf("__keyspace@%d__:{*}%s", db, clientCloseKeySuffix)
}

// The client id of a keyspace channel matching `ClientCloseKeyEventPattern`.
func ParseClientCloseKeyEvent(channel string) (server.Id, bool) {
	return parseClientKeyEvent(channel, clientCloseKeySuffix)
}

func parseClientKeyEvent(channel string, suffix string) (clientId server.Id, ok bool) {
	i := strings.Index(channel, ":{")
	if i < 0 {
		return
	}
	rest := channel[i+len(":{"):]
	end := "}" + suffix
	if !strings.HasSuffix(rest, end) {
		return
	}
	clientId, err := server.ParseId(rest[:len(rest)-len(end)])
	if err != nil {
		return
	}
	ok = true
	return
}

// Sets the removed flag of each client. Each SET is one slot.
func SetClientsRemoved(ctx context.Context, clientIds []server.Id) {
	removeTimeMillis := server.NowUtc().UnixMilli()
	forEachReapedClientRedisChunk(clientIds, func(chunk []server.Id) {
		server.Redis(ctx, func(r server.RedisClient) {
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, clientId := range chunk {
					pipe.Set(ctx, clientRemovedKey(clientId), removeTimeMillis, ClientRemovedTtl)
				}
				return nil
			})
			server.Raise(err)
		})
	})
}

// Whether the client's removed flag is set. A process that may have missed
// the flag's event checks the clients it holds.
func IsClientRemoved(ctx context.Context, clientId server.Id) (removed bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		count, err := r.Exists(ctx, clientRemovedKey(clientId)).Result()
		server.Raise(err)
		removed = 0 < count
	})
	return
}

type ClientCloseRequests struct {
	ContractIds []server.Id
	PeerIds     []server.Id
}

func (self *ClientCloseRequests) Empty() bool {
	return len(self.ContractIds) == 0 && len(self.PeerIds) == 0
}

// Adds close requests for the client and renews the requests' ttl, in one
// script on the client's slot.
func AddClientCloseRequests(ctx context.Context, clientId server.Id, contractIds []server.Id, peerIds []server.Id) {
	if len(contractIds) == 0 && len(peerIds) == 0 {
		return
	}
	args := make([]any, 0, 1+len(contractIds)+len(peerIds))
	args = append(args, ClientCloseTtl.Milliseconds())
	for _, contractId := range contractIds {
		args = append(args, clientCloseContractPrefix+contractId.String())
	}
	for _, peerId := range peerIds {
		args = append(args, clientClosePeerPrefix+peerId.String())
	}
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Eval(ctx, `
			for i = 2, #ARGV do
				redis.call('SADD', KEYS[1], ARGV[i])
			end
			redis.call('PEXPIRE', KEYS[1], ARGV[1])
			return 0
		`, []string{clientCloseKey(clientId)}, args...).Err())
	})
}

// Takes up to `limit` close requests of the client. SPOP gives each request to
// exactly one taker.
func TakeClientCloseRequests(ctx context.Context, clientId server.Id, limit int) *ClientCloseRequests {
	requests := &ClientCloseRequests{}
	server.Redis(ctx, func(r server.RedisClient) {
		members, err := r.SPopN(ctx, clientCloseKey(clientId), int64(limit)).Result()
		if err == redis.Nil {
			return
		}
		server.Raise(err)
		for _, member := range members {
			switch {
			case strings.HasPrefix(member, clientCloseContractPrefix):
				if contractId, err := server.ParseId(member[len(clientCloseContractPrefix):]); err == nil {
					requests.ContractIds = append(requests.ContractIds, contractId)
				}
			case strings.HasPrefix(member, clientClosePeerPrefix):
				if peerId, err := server.ParseId(member[len(clientClosePeerPrefix):]); err == nil {
					requests.PeerIds = append(requests.PeerIds, peerId)
				}
			}
		}
	})
	return requests
}

// After the sender's final close commits, asks the destination to send its
// final for the contract (CLIENTCLOSE1.md section 3).
func addClientCloseRelayPostCommit(ctx context.Context, tx server.PgTx, destinationId server.Id, contractId server.Id) {
	server.AddTxPostCommit(tx, "client_close_relay/"+contractId.String(), func() any {
		relayCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), server.TxPostCommitTimeout)
		defer cancel()
		server.HandleError(func() {
			AddClientCloseRequests(relayCtx, destinationId, []server.Id{contractId}, nil)
		})
		return nil
	})
}

// After an api removal commits, sets the removed flags and queues the read
// that asks the clients' counterparties to close their open contracts with
// them (CLIENTCLOSE1.md section 4).
func addClientsRemovedPostCommit(ctx context.Context, tx server.PgTx, clientIds []server.Id) {
	removedClientIds := slices.Clone(clientIds)
	server.AddTxPostCommit(tx, "client_close_removed/"+server.NewId().String(), func() any {
		removedCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), server.TxPostCommitTimeout)
		defer cancel()
		server.HandleError(func() {
			SetClientsRemoved(removedCtx, removedClientIds)
		})
		for _, clientId := range removedClientIds {
			queueClientRemovedClose(clientId)
		}
		return nil
	})
}

var clientRemovedQueue = sync.OnceValue(func() chan server.Id {
	queue := make(chan server.Id, clientRemovedQueueSize)
	for range clientRemovedWorkerCount {
		go func() {
			for clientId := range queue {
				server.HandleError(func() {
					ctx, cancel := context.WithTimeout(context.Background(), clientRemovedReadTimeout)
					defer cancel()
					RequestRemovedClientContractCloses(ctx, clientId)
				})
			}
		}()
	}
	return queue
})

func queueClientRemovedClose(clientId server.Id) {
	select {
	case clientRemovedQueue() <- clientId:
	default:
		glog.V(1).Infof("[ccm]removal close queue full; contracts left to the sweep\n")
	}
}

// Asks each counterparty of the removed client's open contracts to close
// them, and every sequence with the client. Reads at most
// `clientRemovedOpenContractLimit` contracts in each direction.
func RequestRemovedClientContractCloses(ctx context.Context, clientId server.Id) {
	peerContractIds := map[server.Id][]server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		// each arm is a range of its endpoint's unresolved pair index
		result, err := conn.Query(
			ctx,
			`
				(
					SELECT contract_id, destination_id
					FROM transfer_contract
					WHERE
						(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
						source_id = $1 AND
						destination_id IS NOT NULL
					LIMIT $2
				)
				UNION ALL
				(
					SELECT contract_id, source_id
					FROM transfer_contract
					WHERE
						(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
						destination_id = $1 AND
						source_id IS NOT NULL
					LIMIT $2
				)
			`,
			clientId,
			clientRemovedOpenContractLimit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId server.Id
				var peerId server.Id
				server.Raise(result.Scan(&contractId, &peerId))
				peerContractIds[peerId] = append(peerContractIds[peerId], contractId)
			}
		})
	})
	for peerId, contractIds := range peerContractIds {
		if peerId == clientId {
			continue
		}
		server.HandleError(func() {
			AddClientCloseRequests(ctx, peerId, contractIds, []server.Id{clientId})
		})
	}
}
