package model

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// DB-backed tests (postgres + redis). Under the owner rule these run only
// after the branch, with its migrations, is merged to main.

func readClientDataUsageCounter(ctx context.Context, clientId server.Id, blockNumber int64) (byteCount ByteCount) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.HGet(ctx, clientDataUsageKey(blockNumber, clientDataUsageShard(clientId)), string(clientId.Bytes())).Result()
		if err == redis.Nil {
			return
		}
		if err != nil {
			panic(err)
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			panic(err)
		}
		byteCount = ByteCount(parsed)
	})
	return
}

// Settling a contract meters the paying client's billable bytes exactly once,
// through the post-commit hook in settleEscrowWithOptionsInTx, and the rollup
// carries them into the client's monthly usage. The provider is not metered.
func TestSettlementMetersThePayingClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ResetClientDataCapState()

		payer := newDataCapTestNetwork(ctx, "embed-payer")
		payerClientId := payer.provisionClient(t, "user:alice:laptop", nil)
		err := AddBasicTransferBalance(
			ctx,
			payer.networkId,
			ByteCount(1024*1024*1024),
			server.NowUtc().Add(-time.Hour),
			server.NowUtc().Add(365*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)

		provider := newDataCapTestNetwork(ctx, "embed-provider")
		providerClientId := provider.provisionClient(t, "provider", nil)

		usedTransferByteCount := ByteCount(4096)
		start := server.NowUtc()
		transferEscrow, err := CreateTransferEscrow(ctx, payer.networkId, payerClientId, provider.networkId, providerClientId, 1024*1024)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, CloseContract(ctx, transferEscrow.ContractId, payerClientId, usedTransferByteCount, false), nil)
		connect.AssertEqual(t, CloseContract(ctx, transferEscrow.ContractId, providerClientId, usedTransferByteCount, false), nil)
		end := server.NowUtc()

		// the counter for the settlement's block holds the payer's bytes
		meteredByteCount := ByteCount(0)
		for blockNumber := clientDataUsageBlockNumber(start); blockNumber <= clientDataUsageBlockNumber(end); blockNumber += 1 {
			meteredByteCount += readClientDataUsageCounter(ctx, payerClientId, blockNumber)
			connect.AssertEqual(t, readClientDataUsageCounter(ctx, providerClientId, blockNumber), ByteCount(0))
		}
		connect.AssertEqual(t, meteredByteCount, usedTransferByteCount)

		// once the block closes the rollup moves it into monthly usage, which
		// the payer's own token reads
		RollupClientDataUsage(ctx, end.Add(2*ClientDataUsageBlockDuration))
		result, err := GetClientDataCap(&GetClientDataCapArgs{}, payer.clientSession(ctx, payerClientId))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*ClientDataCapError)(nil))
		connect.AssertEqual(t, result.MonthlyUsedByteCount, usedTransferByteCount)
		// no cap was ever requested: no running total
		connect.AssertEqual(t, result.TotalPeriodStart, (*time.Time)(nil))
		connect.AssertEqual(t, result.Capped, false)
	})
}

// A network's credentials reach only its own clients: another network's
// client is not found for set, get and list.
func TestClientDataCapForeignNetworkIsNotFound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ResetClientDataCapState()

		network := newDataCapTestNetwork(ctx, "embed")
		foreign := newDataCapTestNetwork(ctx, "other")
		foreignClientId := foreign.provisionClient(t, "user:mallory", nil)
		setDataCapJson(t, foreign.rootSession, `{"client_id":"`+foreignClientId.String()+`","monthly_byte_limit":1000}`)

		for _, networkSession := range []struct {
			name    string
			session *session.ClientSession
		}{
			{name: "root", session: network.rootSession},
			{name: "api key", session: network.apiKeySession},
		} {
			setResult := setDataCapJson(t, networkSession.session, `{"client_id":"`+foreignClientId.String()+`","monthly_byte_limit":0}`)
			if setResult.Error == nil || setResult.Error.Message != "Client not found in this network." {
				t.Fatalf("%s set on a foreign client = %+v", networkSession.name, setResult.Error)
			}
			getResult, err := GetClientDataCap(&GetClientDataCapArgs{ClientId: foreignClientId.String()}, networkSession.session)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, getResult.Error.Message, "Client not found in this network.")

			listResult, err := ListClientDataCaps(&ListClientDataCapsArgs{}, networkSession.session)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, listResult.Error, (*ClientDataCapError)(nil))
			for _, client := range listResult.Clients {
				connect.AssertNotEqual(t, client.ClientId, foreignClientId)
			}
		}

		// the foreign cap is unchanged by the refused requests
		getResult, err := GetClientDataCap(&GetClientDataCapArgs{ClientId: foreignClientId.String()}, foreign.rootSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *getResult.MonthlyByteLimit, ByteCount(1000))

		// a client token of one network cannot name a client of another
		ownClientId := network.provisionClient(t, "user:alice", nil)
		getResult, err = GetClientDataCap(&GetClientDataCapArgs{ClientId: foreignClientId.String()}, network.clientSession(ctx, ownClientId))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, getResult.Error.Message, "A client token can only read its own data cap.")
	})
}
