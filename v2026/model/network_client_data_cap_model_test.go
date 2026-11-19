package model

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/session"
)

// DB-backed tests (postgres + redis). Under the owner rule these run only
// after the branch, with its migrations, is merged to main.

type dataCapTestNetwork struct {
	networkId     server.Id
	userId        server.Id
	rootSession   *session.ClientSession
	apiKeySession *session.ClientSession
}

// newDataCapTestNetwork creates an Embed-enabled network: the data-cap and
// ACL-group APIs refuse every other network (network_embed_model.go).
func newDataCapTestNetwork(ctx context.Context, name string) *dataCapTestNetwork {
	network := newDataCapTestNetworkWithoutEmbed(ctx, name)
	Testing_EnableNetworkEmbed(ctx, network.networkId)
	return network
}

// newDataCapTestNetworkWithoutEmbed creates a network that is not
// Embed-enabled.
func newDataCapTestNetworkWithoutEmbed(ctx context.Context, name string) *dataCapTestNetwork {
	networkId := server.NewId()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, name, userId)
	return &dataCapTestNetwork{
		networkId:   networkId,
		userId:      userId,
		rootSession: session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId}),
		// an API key session as session/client_session.go builds it: no
		// client id, pro mode off
		apiKeySession: session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, name, false, false)),
	}
}

func (self *dataCapTestNetwork) provisionClient(t testing.TB, description string, sourceClientId *server.Id) server.Id {
	result, err := AuthNetworkClient(&AuthNetworkClientArgs{
		Description:    description,
		SourceClientId: sourceClientId,
	}, self.rootSession)
	connect.AssertEqual(t, err, nil)
	if result.Error != nil {
		t.Fatalf("provision %s refused: %s", description, result.Error.Message)
	}
	return *result.ClientId
}

func (self *dataCapTestNetwork) clientSession(ctx context.Context, clientId server.Id) *session.ClientSession {
	return session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: self.networkId,
		UserId:    self.userId,
		ClientId:  &clientId,
	})
}

func setDataCapJson(t testing.TB, clientSession *session.ClientSession, body string) *ClientDataCapResult {
	args := &SetClientDataCapArgs{}
	if err := json.Unmarshal([]byte(body), args); err != nil {
		t.Fatalf("decode %s: %s", body, err)
	}
	result, err := SetClientDataCap(args, clientSession)
	connect.AssertEqual(t, err, nil)
	return result
}

func TestSetClientDataCapMergeSemantics(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ResetClientDataCapState()

		network := newDataCapTestNetwork(ctx, "embed")
		clientId := network.provisionClient(t, "user:alice", nil)
		clientIdJson := `"client_id":"` + clientId.String() + `"`

		// set monthly with an API key session; the omitted total stays unset
		result := setDataCapJson(t, network.apiKeySession, `{`+clientIdJson+`,"monthly_byte_limit":1000}`)
		connect.AssertEqual(t, result.Error, (*ClientDataCapError)(nil))
		connect.AssertEqual(t, *result.MonthlyByteLimit, ByteCount(1000))
		connect.AssertEqual(t, result.TotalByteLimit == nil, true)
		connect.AssertEqual(t, result.Capped, false)
		connect.AssertNotEqual(t, result.TotalPeriodStart, (*time.Time)(nil))
		firstTotalPeriodStart := *result.TotalPeriodStart

		// set total with the root token; the omitted monthly is kept
		result = setDataCapJson(t, network.rootSession, `{`+clientIdJson+`,"total_byte_limit":500}`)
		connect.AssertEqual(t, *result.MonthlyByteLimit, ByteCount(1000))
		connect.AssertEqual(t, *result.TotalByteLimit, ByteCount(500))

		// null clears only that cap
		result = setDataCapJson(t, network.apiKeySession, `{`+clientIdJson+`,"monthly_byte_limit":null}`)
		connect.AssertEqual(t, result.MonthlyByteLimit == nil, true)
		connect.AssertEqual(t, *result.TotalByteLimit, ByteCount(500))

		// zero pauses at once
		result = setDataCapJson(t, network.apiKeySession, `{`+clientIdJson+`,"total_byte_limit":0}`)
		connect.AssertEqual(t, result.Capped, true)
		connect.AssertEqual(t, result.CappedReason, ClientDataCapReasonTotal)
		result = setDataCapJson(t, network.apiKeySession, `{`+clientIdJson+`,"total_byte_limit":null,"monthly_byte_limit":0}`)
		connect.AssertEqual(t, result.Capped, true)
		connect.AssertEqual(t, result.CappedReason, ClientDataCapReasonMonthly)

		// reset starts a new total period
		time.Sleep(10 * time.Millisecond)
		result = setDataCapJson(t, network.apiKeySession, `{`+clientIdJson+`,"monthly_byte_limit":null,"reset_total":true}`)
		connect.AssertEqual(t, result.Capped, false)
		connect.AssertEqual(t, result.TotalUsedByteCount, ByteCount(0))
		connect.AssertEqual(t, result.TotalPeriodStart.After(firstTotalPeriodStart), true)

		// refusals
		childId := network.provisionClient(t, "user:alice:window", &clientId)
		result = setDataCapJson(t, network.apiKeySession, `{"client_id":"`+childId.String()+`","monthly_byte_limit":1}`)
		connect.AssertEqual(t, result.Error.Message, "Data caps apply to top-level clients.")
		result = setDataCapJson(t, network.apiKeySession, `{"client_id":"`+server.NewId().String()+`","monthly_byte_limit":1}`)
		connect.AssertEqual(t, result.Error.Message, "Client not found in this network.")
		otherNetwork := newDataCapTestNetwork(ctx, "other")
		otherClientId := otherNetwork.provisionClient(t, "user:mallory", nil)
		result = setDataCapJson(t, network.apiKeySession, `{"client_id":"`+otherClientId.String()+`","monthly_byte_limit":1}`)
		connect.AssertEqual(t, result.Error.Message, "Client not found in this network.")
		result = setDataCapJson(t, network.clientSession(ctx, clientId), `{`+clientIdJson+`,"monthly_byte_limit":1}`)
		connect.AssertEqual(t, result.Error.Message, clientDataCapNetworkSessionMessage)
	})
}

func TestGetClientDataCapAuth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ResetClientDataCapState()

		network := newDataCapTestNetwork(ctx, "embed")
		clientId := network.provisionClient(t, "user:alice", nil)
		otherClientId := network.provisionClient(t, "user:bob", nil)
		childId := network.provisionClient(t, "user:alice:window", &clientId)
		setDataCapJson(t, network.apiKeySession, `{"client_id":"`+clientId.String()+`","monthly_byte_limit":2000}`)

		get := func(clientSession *session.ClientSession, clientIdArg string) *ClientDataCapResult {
			result, err := GetClientDataCap(&GetClientDataCapArgs{ClientId: clientIdArg}, clientSession)
			connect.AssertEqual(t, err, nil)
			return result
		}

		// the network, by root token or API key
		for _, networkSession := range []*session.ClientSession{network.rootSession, network.apiKeySession} {
			result := get(networkSession, clientId.String())
			connect.AssertEqual(t, result.Error, (*ClientDataCapError)(nil))
			connect.AssertEqual(t, result.ClientId, clientId)
			connect.AssertEqual(t, *result.MonthlyByteLimit, ByteCount(2000))
			connect.AssertEqual(t, get(networkSession, "").Error.Message, "client_id is required.")
			connect.AssertEqual(t, get(networkSession, childId.String()).Error.Message, "Data caps apply to top-level clients.")
		}

		// a client without a cap request reads usage and null limits
		result := get(network.apiKeySession, otherClientId.String())
		connect.AssertEqual(t, result.MonthlyByteLimit == nil, true)
		connect.AssertEqual(t, result.TotalPeriodStart == nil, true)

		// a client token reads its own, with or without its id
		clientSession := network.clientSession(ctx, clientId)
		connect.AssertEqual(t, get(clientSession, "").ClientId, clientId)
		connect.AssertEqual(t, get(clientSession, clientId.String()).ClientId, clientId)
		connect.AssertEqual(t, get(clientSession, otherClientId.String()).Error.Message, "A client token can only read its own data cap.")

		// a child client's token reads its top-level client's cap
		childSession := network.clientSession(ctx, childId)
		result = get(childSession, "")
		connect.AssertEqual(t, result.ClientId, clientId)
		connect.AssertEqual(t, *result.MonthlyByteLimit, ByteCount(2000))
		connect.AssertEqual(t, get(childSession, clientId.String()).ClientId, clientId)
	})
}

func TestListClientDataCapsPaging(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ResetClientDataCapState()

		network := newDataCapTestNetwork(ctx, "embed")
		cappedClientIds := []server.Id{}
		for _, body := range []string{
			`"monthly_byte_limit":1000`,
			`"total_byte_limit":2000`,
			`"monthly_byte_limit":0`,
			`"monthly_byte_limit":3000,"total_byte_limit":4000`,
		} {
			clientId := network.provisionClient(t, "capped", nil)
			setDataCapJson(t, network.apiKeySession, `{"client_id":"`+clientId.String()+`",`+body+`}`)
			cappedClientIds = append(cappedClientIds, clientId)
		}
		// a client whose caps were all cleared is not listed
		clearedClientId := network.provisionClient(t, "cleared", nil)
		setDataCapJson(t, network.apiKeySession, `{"client_id":"`+clearedClientId.String()+`","monthly_byte_limit":1}`)
		setDataCapJson(t, network.apiKeySession, `{"client_id":"`+clearedClientId.String()+`","monthly_byte_limit":null}`)
		// another network's caps are not listed
		otherNetwork := newDataCapTestNetwork(ctx, "other")
		otherClientId := otherNetwork.provisionClient(t, "other", nil)
		setDataCapJson(t, otherNetwork.apiKeySession, `{"client_id":"`+otherClientId.String()+`","monthly_byte_limit":1}`)

		listed := []server.Id{}
		cursor := ""
		pageCount := 0
		for {
			result, err := ListClientDataCaps(&ListClientDataCapsArgs{Cursor: cursor, Limit: "3"}, network.apiKeySession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, (*ClientDataCapError)(nil))
			for _, clientDataCap := range result.Clients {
				listed = append(listed, clientDataCap.ClientId)
			}
			pageCount += 1
			if result.NextCursor == nil {
				break
			}
			cursor = *result.NextCursor
		}
		connect.AssertEqual(t, pageCount, 2)

		slices.SortFunc(cappedClientIds, func(a, b server.Id) int { return a.Cmp(b) })
		connect.AssertEqual(t, listed, cappedClientIds)

		// the root token lists too; a client token does not
		result, err := ListClientDataCaps(&ListClientDataCapsArgs{}, network.rootSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.Clients), len(cappedClientIds))
		connect.AssertEqual(t, result.NextCursor == nil, true)
		result, err = ListClientDataCaps(&ListClientDataCapsArgs{}, network.clientSession(ctx, cappedClientIds[0]))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Message, clientDataCapNetworkSessionMessage)
	})
}

func TestClientDataCapEscrowAdmission(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ResetClientDataCapState()

		network := newDataCapTestNetwork(ctx, "embed")
		clientId := network.provisionClient(t, "user:alice", nil)
		childId := network.provisionClient(t, "user:alice:window", &clientId)
		otherNetwork := newDataCapTestNetwork(ctx, "other")
		otherClientId := otherNetwork.provisionClient(t, "user:bob", nil)

		admission := func(payerNetworkId server.Id, payerClientId server.Id, byteCount ByteCount) (err error) {
			server.Db(ctx, func(conn server.PgConn) {
				err = clientDataCapEscrowError(ctx, conn, payerNetworkId, payerClientId, byteCount, server.NowUtc())
			})
			return
		}

		// not capped
		connect.AssertEqual(t, admission(network.networkId, clientId, 1024), nil)

		// paused: the client and its child are refused like an empty balance
		setDataCapJson(t, network.apiKeySession, `{"client_id":"`+clientId.String()+`","monthly_byte_limit":0}`)
		err := admission(network.networkId, clientId, 1024)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, err.Error(), "Insufficient balance (0).")
		connect.AssertNotEqual(t, admission(network.networkId, childId, 1024), nil)
		// zero-byte contracts keep working
		connect.AssertEqual(t, admission(network.networkId, clientId, 0), nil)
		// another network's payer is untouched
		connect.AssertEqual(t, admission(otherNetwork.networkId, otherClientId, 1024), nil)

		// clearing the cap admits again (this host invalidated its snapshot)
		setDataCapJson(t, network.apiKeySession, `{"client_id":"`+clientId.String()+`","monthly_byte_limit":null}`)
		connect.AssertEqual(t, admission(network.networkId, clientId, 1024), nil)
		connect.AssertEqual(t, admission(network.networkId, childId, 1024), nil)
	})
}

func TestRollupClientDataUsage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		Testing_ResetClientDataCapState()

		network := newDataCapTestNetwork(ctx, "embed")
		clientId := network.provisionClient(t, "user:alice", nil)
		childId := network.provisionClient(t, "user:alice:window", &clientId)
		uncappedClientId := network.provisionClient(t, "user:bob", nil)
		setDataCapJson(t, network.apiKeySession, `{"client_id":"`+clientId.String()+`","total_byte_limit":1000,"monthly_byte_limit":50000}`)
		capTime := server.NowUtc()

		readUsage := func(clientId server.Id, monthStart time.Time) (usedByteCount ByteCount) {
			server.Db(ctx, func(conn server.PgConn) {
				usedByteCount = readClientDataUsage(ctx, conn, clientId, monthStart)
			})
			return
		}
		readRow := func(clientId server.Id) (row *clientDataCapRow) {
			server.Db(ctx, func(conn server.PgConn) {
				row = readClientDataCapRow(ctx, conn, clientId, false)
			})
			return
		}

		// a closed block that started before the first cap request counts for
		// the month but not for the running total, which starts at that request
		preCapTime := capTime.Add(-5 * ClientDataUsageBlockDuration)
		RecordClientDataUsage(ctx, clientId, 200, preCapTime)
		RollupClientDataUsage(ctx, server.NowUtc())
		connect.AssertEqual(t, readUsage(clientId, clientDataCapMonthStart(preCapTime)), ByteCount(200))
		connect.AssertEqual(t, readRow(clientId).totalUsedByteCount, ByteCount(0))

		// a block that starts after the cap request, rolled up once it has closed
		usageTime := capTime.Add(ClientDataUsageBlockDuration)
		monthStart := clientDataCapMonthStart(usageTime)
		RecordClientDataUsage(ctx, clientId, 600, usageTime)
		RecordClientDataUsage(ctx, childId, 500, usageTime)
		RecordClientDataUsage(ctx, uncappedClientId, 70, usageTime)
		RollupClientDataUsage(ctx, usageTime.Add(3*ClientDataUsageBlockDuration))

		monthUsage := ByteCount(1100)
		if clientDataCapMonthStart(preCapTime).Equal(monthStart) {
			monthUsage += 200
		}

		// the child's bytes count for its top-level client; clients without a
		// cap are metered too
		connect.AssertEqual(t, readUsage(clientId, monthStart), monthUsage)
		connect.AssertEqual(t, readUsage(childId, monthStart), ByteCount(0))
		connect.AssertEqual(t, readUsage(uncappedClientId, monthStart), ByteCount(70))
		row := readRow(clientId)
		connect.AssertEqual(t, row.totalUsedByteCount, ByteCount(1100))
		connect.AssertEqual(t, row.totalCapped, true)

		// a capped marker reaches admission through the snapshot
		Testing_ResetClientDataCapState()
		var err error
		server.Db(ctx, func(conn server.PgConn) {
			err = clientDataCapEscrowError(ctx, conn, network.networkId, childId, 1, server.NowUtc())
		})
		connect.AssertNotEqual(t, err, nil)

		// a drained (block, shard) applies once: a hash that reappears is skipped
		blockNumber := clientDataUsageBlockNumber(usageTime)
		RecordClientDataUsage(ctx, clientId, 600, usageTime)
		drainClientDataUsageShard(ctx, blockNumber, clientDataUsageShard(clientId))
		connect.AssertEqual(t, readUsage(clientId, monthStart), monthUsage)
		connect.AssertEqual(t, readRow(clientId).totalUsedByteCount, ByteCount(1100))

		// after a reset, a block that started before it counts for the month
		// but not for the new total
		result := setDataCapJson(t, network.apiKeySession, `{"client_id":"`+clientId.String()+`","reset_total":true}`)
		connect.AssertEqual(t, result.Capped, false)
		earlierTime := server.NowUtc().Add(-4 * ClientDataUsageBlockDuration)
		RecordClientDataUsage(ctx, clientId, 300, earlierTime)
		drainClientDataUsageShard(ctx, clientDataUsageBlockNumber(earlierTime), clientDataUsageShard(clientId))
		connect.AssertEqual(t, readUsage(clientId, clientDataCapMonthStart(earlierTime)) >= 300, true)
		connect.AssertEqual(t, readRow(clientId).totalUsedByteCount, ByteCount(0))

		// a block that starts after the reset counts for the total and caps it
		// again (a block that merely contains the reset started before it)
		laterTime := server.NowUtc().Add(2 * ClientDataUsageBlockDuration)
		RecordClientDataUsage(ctx, clientId, 1000, laterTime)
		drainClientDataUsageShard(ctx, clientDataUsageBlockNumber(laterTime), clientDataUsageShard(clientId))
		row = readRow(clientId)
		connect.AssertEqual(t, row.totalUsedByteCount, ByteCount(1000))
		connect.AssertEqual(t, row.totalCapped, true)
	})
}

func TestNetworkTopLevelClientLimitOverride(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer Testing_SetEnforceConcurrentClients(true)()
		// no connected client limit, so only the top-level cap can refuse
		defer Testing_SetConcurrentClientsLimit(0, 0)()

		network := newDataCapTestNetwork(ctx, "embed")
		provision := func() *AuthNetworkClientResult {
			result, err := AuthNetworkClient(&AuthNetworkClientArgs{Description: "user"}, network.rootSession)
			connect.AssertEqual(t, err, nil)
			return result
		}

		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, network.networkId, 3), nil)
		limit := GetNetworkTopLevelClientLimit(ctx, network.networkId)
		connect.AssertEqual(t, limit.Limit, 3)
		connect.AssertEqual(t, limit.Override, true)
		for range 3 {
			connect.AssertEqual(t, provision().Error, (*AuthNetworkClientError)(nil))
		}
		result := provision()
		if result.Error == nil || !result.Error.ClientLimitExceeded {
			t.Fatal("the per-network limit did not refuse")
		}

		// an Embed plan raises the limit
		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, network.networkId, 5), nil)
		connect.AssertEqual(t, provision().Error, (*AuthNetworkClientError)(nil))
		connect.AssertEqual(t, provision().Error, (*AuthNetworkClientError)(nil))
		result = provision()
		if result.Error == nil || !result.Error.ClientLimitExceeded {
			t.Fatal("the raised limit did not refuse")
		}

		// clearing returns to the default
		ClearNetworkTopLevelClientLimit(ctx, network.networkId)
		limit = GetNetworkTopLevelClientLimit(ctx, network.networkId)
		connect.AssertEqual(t, limit.Limit, LimitTopLevelClientIdsPerNetwork)
		connect.AssertEqual(t, limit.Override, false)
		connect.AssertEqual(t, provision().Error, (*AuthNetworkClientError)(nil))

		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, server.NewId(), 10), ErrNetworkNotFound)
		connect.AssertNotEqual(t, SetNetworkTopLevelClientLimit(ctx, network.networkId, 0), nil)
		connect.AssertNotEqual(t, SetNetworkTopLevelClientLimit(ctx, network.networkId, MaxNetworkTopLevelClientLimit+1), nil)

		// the peer valve keeps the constant
		connect.AssertEqual(t, LimitTopLevelClientIdsPerNetwork, 100)
	})
}
