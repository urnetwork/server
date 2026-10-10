package model

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Pure tests: no database or redis.

func TestSetClientDataCapArgsMergeJson(t *testing.T) {
	clientId := server.NewId()
	decode := func(body string) *SetClientDataCapArgs {
		args := &SetClientDataCapArgs{}
		if err := json.Unmarshal([]byte(body), args); err != nil {
			t.Fatalf("decode %s: %s", body, err)
		}
		return args
	}

	// omitted: keep
	args := decode(`{"client_id":"` + clientId.String() + `"}`)
	connect.AssertEqual(t, args.ClientId, clientId)
	connect.AssertEqual(t, args.monthlyByteLimitSet(), false)
	connect.AssertEqual(t, args.totalByteLimitSet(), false)
	connect.AssertEqual(t, args.ResetTotal, false)

	// null: clear
	args = decode(`{"client_id":"` + clientId.String() + `","monthly_byte_limit":null}`)
	connect.AssertEqual(t, args.monthlyByteLimitSet(), true)
	connect.AssertEqual(t, args.MonthlyByteLimit == nil, true)
	connect.AssertEqual(t, args.totalByteLimitSet(), false)

	// numbers: set, and zero is a value (paused), not a clear
	args = decode(`{"client_id":"` + clientId.String() + `","monthly_byte_limit":5000000000,"total_byte_limit":0,"reset_total":true}`)
	connect.AssertEqual(t, args.monthlyByteLimitSet(), true)
	connect.AssertEqual(t, *args.MonthlyByteLimit, ByteCount(5000000000))
	connect.AssertEqual(t, args.totalByteLimitSet(), true)
	connect.AssertEqual(t, *args.TotalByteLimit, ByteCount(0))
	connect.AssertEqual(t, args.ResetTotal, true)

	// a reused value forgets the limits an earlier decode named
	args = decode(`{"client_id":"` + clientId.String() + `","total_byte_limit":null}`)
	connect.AssertEqual(t, json.Unmarshal([]byte(`{"client_id":"`+clientId.String()+`"}`), args), nil)
	connect.AssertEqual(t, args.totalByteLimitSet(), false)

	// not a number
	if err := json.Unmarshal([]byte(`{"monthly_byte_limit":"5"}`), &SetClientDataCapArgs{}); err == nil {
		t.Fatal("a string limit decoded")
	}
	if err := json.Unmarshal([]byte(`{"monthly_byte_limit":1.5}`), &SetClientDataCapArgs{}); err == nil {
		t.Fatal("a fractional limit decoded")
	}

	// in Go a non-nil limit sets, and the setter clears with nil
	five := ByteCount(5)
	args = &SetClientDataCapArgs{ClientId: clientId, MonthlyByteLimit: &five}
	connect.AssertEqual(t, args.monthlyByteLimitSet(), true)
	connect.AssertEqual(t, args.totalByteLimitSet(), false)
	args.SetTotalByteLimit(nil)
	connect.AssertEqual(t, args.totalByteLimitSet(), true)
	connect.AssertEqual(t, args.TotalByteLimit == nil, true)
}

func TestMergeClientDataCapLimit(t *testing.T) {
	current := ByteCount(100)
	update := ByteCount(200)
	zero := ByteCount(0)

	connect.AssertEqual(t, *mergeClientDataCapLimit(&current, false, nil), ByteCount(100))
	connect.AssertEqual(t, *mergeClientDataCapLimit(&current, false, &update), ByteCount(100))
	connect.AssertEqual(t, mergeClientDataCapLimit(nil, false, nil) == nil, true)
	connect.AssertEqual(t, mergeClientDataCapLimit(&current, true, nil) == nil, true)
	connect.AssertEqual(t, *mergeClientDataCapLimit(&current, true, &update), ByteCount(200))
	connect.AssertEqual(t, *mergeClientDataCapLimit(nil, true, &zero), ByteCount(0))

	// the merged value does not alias the request
	merged := mergeClientDataCapLimit(nil, true, &update)
	update = 300
	connect.AssertEqual(t, *merged, ByteCount(200))
}

func TestClientDataCapLimitMessage(t *testing.T) {
	negative := ByteCount(-1)
	zero := ByteCount(0)
	limit := MaxClientDataCapByteLimit
	over := MaxClientDataCapByteLimit + 1

	connect.AssertEqual(t, clientDataCapLimitMessage(nil), "")
	connect.AssertEqual(t, clientDataCapLimitMessage(&zero), "")
	connect.AssertEqual(t, clientDataCapLimitMessage(&limit), "")
	connect.AssertNotEqual(t, clientDataCapLimitMessage(&negative), "")
	connect.AssertNotEqual(t, clientDataCapLimitMessage(&over), "")
}

func TestClientDataCapReason(t *testing.T) {
	limit := func(v ByteCount) *ByteCount {
		return &v
	}
	capped, reason := clientDataCapReason(nil, 1000, nil, 1000)
	connect.AssertEqual(t, capped, false)
	connect.AssertEqual(t, reason, "")

	capped, reason = clientDataCapReason(limit(1000), 999, nil, 0)
	connect.AssertEqual(t, capped, false)
	connect.AssertEqual(t, reason, "")

	capped, reason = clientDataCapReason(limit(1000), 1000, nil, 0)
	connect.AssertEqual(t, capped, true)
	connect.AssertEqual(t, reason, ClientDataCapReasonMonthly)

	capped, reason = clientDataCapReason(nil, 0, limit(500), 501)
	connect.AssertEqual(t, capped, true)
	connect.AssertEqual(t, reason, ClientDataCapReasonTotal)

	// both reached: the total wins, since only it outlasts the month
	capped, reason = clientDataCapReason(limit(10), 20, limit(10), 20)
	connect.AssertEqual(t, capped, true)
	connect.AssertEqual(t, reason, ClientDataCapReasonTotal)

	// zero pauses with no usage at all
	capped, reason = clientDataCapReason(limit(0), 0, nil, 0)
	connect.AssertEqual(t, capped, true)
	connect.AssertEqual(t, reason, ClientDataCapReasonMonthly)
}

func TestClientDataCapMonth(t *testing.T) {
	cases := []struct {
		at    time.Time
		start time.Time
		end   time.Time
	}{
		{
			at:    time.Date(2026, 10, 8, 13, 41, 2, 7, time.UTC),
			start: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			at:    time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
			start: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			at:    time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC),
			start: time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			// a local time just past midnight on the 1st is still the prior UTC month
			at:    time.Date(2026, 11, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600)),
			start: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		start := clientDataCapMonthStart(c.at)
		connect.AssertEqual(t, start, c.start)
		connect.AssertEqual(t, clientDataCapMonthEnd(start), c.end)
	}
}

func TestClientDataCapCursor(t *testing.T) {
	clientId := server.NewId()
	cursor := encodeClientDataCapCursor(clientId)
	decoded, ok := decodeClientDataCapCursor(cursor)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, decoded, clientId)

	decoded, ok = decodeClientDataCapCursor("")
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, decoded, server.Id{})

	for _, bad := range []string{"not base64!", "AAAA", clientId.String()} {
		_, ok = decodeClientDataCapCursor(bad)
		connect.AssertEqual(t, ok, false)
	}
}

func TestParseClientDataCapListLimit(t *testing.T) {
	limit, ok := parseClientDataCapListLimit("")
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, limit, DefaultClientDataCapListLimit)
	for _, good := range []int{1, 50, MaxClientDataCapListLimit} {
		limit, ok = parseClientDataCapListLimit(strconv.Itoa(good))
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, limit, good)
	}
	for _, bad := range []string{"0", "-1", strconv.Itoa(MaxClientDataCapListLimit + 1), "ten", "1.5"} {
		_, ok = parseClientDataCapListLimit(bad)
		connect.AssertEqual(t, ok, false)
	}
}

func TestClientDataUsageRollupBlockNumbers(t *testing.T) {
	retained := int64(clientDataUsageRedisTtl / ClientDataUsageBlockDuration)
	current := int64(1_000_000)

	// no high-water mark: every block inside the hash ttl up to the last closed one
	blocks := clientDataUsageRollupBlockNumbers(current, 0, false)
	connect.AssertEqual(t, blocks[0], current-retained-1)
	connect.AssertEqual(t, blocks[len(blocks)-1], current-2)

	// normally one block past the mark
	blocks = clientDataUsageRollupBlockNumbers(current, current-3, true)
	connect.AssertEqual(t, blocks, []int64{current - 2})

	// caught up
	connect.AssertEqual(t, len(clientDataUsageRollupBlockNumbers(current, current-2, true)), 0)

	// an ancient mark stays bounded by the ttl
	blocks = clientDataUsageRollupBlockNumbers(current, 5, true)
	connect.AssertEqual(t, blocks[0], current-retained-1)
}

func TestClientDataUsageBlock(t *testing.T) {
	at := time.Date(2026, 10, 8, 13, 41, 17, 0, time.UTC)
	block := clientDataUsageBlockNumber(at)
	start := clientDataUsageBlockStart(block)
	if start.After(at) || !at.Before(start.Add(ClientDataUsageBlockDuration)) {
		t.Fatalf("block start %s does not contain %s", start, at)
	}
	connect.AssertEqual(t, clientDataUsageBlockNumber(start), block)
	connect.AssertEqual(t, clientDataUsageBlockNumber(start.Add(ClientDataUsageBlockDuration)), block+1)

	for range 100 {
		clientId := server.NewId()
		shard := clientDataUsageShard(clientId)
		if shard < 0 || clientDataUsageShardCount <= shard {
			t.Fatalf("shard out of range: %d", shard)
		}
		connect.AssertEqual(t, clientDataUsageShard(clientId), shard)
	}
}

func TestParseClientDataUsageFields(t *testing.T) {
	a := server.NewId()
	b := server.NewId()
	usage := parseClientDataUsageFields(map[string]string{
		string(a.Bytes()):              "100",
		string(b.Bytes()):              "250",
		"short":                        "7",
		string(server.NewId().Bytes()): "not a number",
		string(server.NewId().Bytes()): "0",
	})
	connect.AssertEqual(t, len(usage), 2)
	connect.AssertEqual(t, usage[a], ByteCount(100))
	connect.AssertEqual(t, usage[b], ByteCount(250))
}

func TestClientDataCapRowRecomputeCapped(t *testing.T) {
	monthStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	limit := func(v ByteCount) *ByteCount {
		return &v
	}

	row := &clientDataCapRow{monthlyByteLimit: limit(1000)}
	row.recomputeCapped(monthStart, 1000)
	connect.AssertEqual(t, *row.monthlyCappedPeriodStart, monthStart)
	connect.AssertEqual(t, row.totalCapped, false)

	// raising the limit clears the monthly marker
	row.monthlyByteLimit = limit(2000)
	row.recomputeCapped(monthStart, 1000)
	connect.AssertEqual(t, row.monthlyCappedPeriodStart == nil, true)

	row = &clientDataCapRow{totalByteLimit: limit(10), totalUsedByteCount: 10}
	row.recomputeCapped(monthStart, 0)
	connect.AssertEqual(t, row.totalCapped, true)

	// a reset total below the limit is no longer capped
	row.totalUsedByteCount = 0
	row.recomputeCapped(monthStart, 0)
	connect.AssertEqual(t, row.totalCapped, false)

	// zero pauses both ways
	row = &clientDataCapRow{monthlyByteLimit: limit(0), totalByteLimit: limit(0)}
	row.recomputeCapped(monthStart, 0)
	connect.AssertEqual(t, *row.monthlyCappedPeriodStart, monthStart)
	connect.AssertEqual(t, row.totalCapped, true)
}

func TestNewClientDataCap(t *testing.T) {
	now := time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)
	clientId := server.NewId()

	// no cap request yet: usage still reads, limits and the total period are null
	clientDataCap := newClientDataCap(clientId, nil, 1234, now)
	connect.AssertEqual(t, clientDataCap.ClientId, clientId)
	connect.AssertEqual(t, clientDataCap.MonthlyByteLimit == nil, true)
	connect.AssertEqual(t, clientDataCap.TotalByteLimit == nil, true)
	connect.AssertEqual(t, clientDataCap.TotalPeriodStart == nil, true)
	connect.AssertEqual(t, clientDataCap.MonthlyUsedByteCount, ByteCount(1234))
	connect.AssertEqual(t, clientDataCap.MonthlyPeriodStart, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, clientDataCap.MonthlyPeriodEnd, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, clientDataCap.Capped, false)
	connect.AssertEqual(t, clientDataCap.CappedReason, "")

	monthly := ByteCount(1000)
	totalPeriodStart := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	clientDataCap = newClientDataCap(clientId, &clientDataCapRow{
		clientId:           clientId,
		monthlyByteLimit:   &monthly,
		totalPeriodStart:   totalPeriodStart,
		totalUsedByteCount: 5000,
	}, 1234, now)
	connect.AssertEqual(t, *clientDataCap.MonthlyByteLimit, ByteCount(1000))
	connect.AssertEqual(t, *clientDataCap.TotalPeriodStart, totalPeriodStart)
	connect.AssertEqual(t, clientDataCap.TotalUsedByteCount, ByteCount(5000))
	connect.AssertEqual(t, clientDataCap.Capped, true)
	connect.AssertEqual(t, clientDataCap.CappedReason, ClientDataCapReasonMonthly)

	// the wire shape: every contract key present, nulls included
	out, err := json.Marshal(&ClientDataCapResult{ClientDataCap: newClientDataCap(clientId, nil, 0, now)})
	connect.AssertEqual(t, err, nil)
	fields := map[string]any{}
	connect.AssertEqual(t, json.Unmarshal(out, &fields), nil)
	for _, key := range []string{
		"client_id", "monthly_byte_limit", "monthly_used_byte_count", "monthly_period_start",
		"monthly_period_end", "total_byte_limit", "total_used_byte_count", "total_period_start",
		"capped", "capped_reason",
	} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing key %s in %s", key, out)
		}
	}
	if _, ok := fields["error"]; ok {
		t.Fatalf("success carries an error: %s", out)
	}

	// a refusal is only the error
	out, err = json.Marshal(clientDataCapErrorResult("no"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(out), `{"error":{"message":"no"}}`)
}

// The routes accept the network's root token and an API key session (built by
// session/client_session.go as session.NewByJwt(..., false, false): no client id,
// pro mode off) as the network; a client token is not the network.
func TestClientDataCapNetworkSession(t *testing.T) {
	networkId := server.NewId()
	// an Embed-enabled network, so these refusals reach the route's own checks
	primeNetworkEmbedCache(t, networkId, true)
	userId := server.NewId()
	clientId := server.NewId()

	apiKeySession := &session.ClientSession{ByJwt: session.NewByJwt(networkId, userId, "embed", false, false)}
	connect.AssertEqual(t, clientDataCapNetworkSession(apiKeySession), true)

	rootSession := &session.ClientSession{ByJwt: &session.ByJwt{NetworkId: networkId, UserId: userId}}
	connect.AssertEqual(t, clientDataCapNetworkSession(rootSession), true)

	clientSession := &session.ClientSession{ByJwt: &session.ByJwt{NetworkId: networkId, UserId: userId, ClientId: &clientId}}
	connect.AssertEqual(t, clientDataCapNetworkSession(clientSession), false)

	connect.AssertEqual(t, clientDataCapNetworkSession(&session.ClientSession{ByJwt: &session.ByJwt{}}), false)
	connect.AssertEqual(t, clientDataCapNetworkSession(&session.ClientSession{}), false)
	connect.AssertEqual(t, clientDataCapNetworkSession(nil), false)

	// the refusals happen before any query
	setResult, err := SetClientDataCap(&SetClientDataCapArgs{ClientId: clientId}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, setResult.Error.Message, clientDataCapNetworkSessionMessage)
	listResult, err := ListClientDataCaps(&ListClientDataCapsArgs{}, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, listResult.Error.Message, clientDataCapNetworkSessionMessage)

	// argument refusals, also before any query
	setResult, err = SetClientDataCap(&SetClientDataCapArgs{}, apiKeySession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, setResult.Error.Message, "client_id is required.")
	negative := ByteCount(-5)
	setResult, err = SetClientDataCap(&SetClientDataCapArgs{
		ClientId:         clientId,
		MonthlyByteLimit: &negative,
	}, apiKeySession)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, setResult.Error, nil)
	listResult, err = ListClientDataCaps(&ListClientDataCapsArgs{Limit: "0"}, apiKeySession)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, listResult.Error, nil)
	listResult, err = ListClientDataCaps(&ListClientDataCapsArgs{Cursor: "%%%"}, apiKeySession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, listResult.Error.Message, "Invalid cursor.")
	getResult, err := GetClientDataCap(&GetClientDataCapArgs{ClientId: "not-a-uuid"}, apiKeySession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, getResult.Error.Message, "Invalid client_id.")
}

// contractOrigin is the endpoint settlement bills; the data usage meter uses
// the same rule, so a contract's bytes count for the client that paid.
func TestContractOrigin(t *testing.T) {
	sourceNetworkId := server.NewId()
	destinationNetworkId := server.NewId()
	sourceId := server.NewId()
	destinationId := server.NewId()
	companionId := server.NewId()
	trueValue := true
	falseValue := false

	cases := []struct {
		name            string
		owner           contractParticipantOwner
		usageOrigin     *bool
		originId        server.Id
		originNetworkId server.Id
		err             bool
	}{
		{
			name:            "forward, legacy row without a payer",
			owner:           contractParticipantOwner{sourceNetworkId: sourceNetworkId, sourceId: sourceId, destinationNetworkId: destinationNetworkId, destinationId: destinationId},
			originId:        sourceId,
			originNetworkId: sourceNetworkId,
		},
		{
			name:            "companion, legacy row without a payer",
			owner:           contractParticipantOwner{sourceNetworkId: sourceNetworkId, sourceId: sourceId, destinationNetworkId: destinationNetworkId, destinationId: destinationId, companionContractId: &companionId},
			originId:        destinationId,
			originNetworkId: destinationNetworkId,
		},
		{
			name:            "companion funded by its destination",
			owner:           contractParticipantOwner{sourceNetworkId: sourceNetworkId, sourceId: sourceId, destinationNetworkId: destinationNetworkId, destinationId: destinationId, payerNetworkId: &destinationNetworkId, companionContractId: &companionId},
			originId:        destinationId,
			originNetworkId: destinationNetworkId,
		},
		{
			name:            "companion funded by an inherited source payer",
			owner:           contractParticipantOwner{sourceNetworkId: sourceNetworkId, sourceId: sourceId, destinationNetworkId: destinationNetworkId, destinationId: destinationId, payerNetworkId: &sourceNetworkId, companionContractId: &companionId},
			originId:        sourceId,
			originNetworkId: sourceNetworkId,
		},
		{
			name:            "same network keeps the companion side",
			owner:           contractParticipantOwner{sourceNetworkId: sourceNetworkId, sourceId: sourceId, destinationNetworkId: sourceNetworkId, destinationId: destinationId, payerNetworkId: &sourceNetworkId, companionContractId: &companionId},
			originId:        destinationId,
			originNetworkId: sourceNetworkId,
		},
		{
			name:            "usage origin overrides the side",
			owner:           contractParticipantOwner{sourceNetworkId: sourceNetworkId, sourceId: sourceId, destinationNetworkId: destinationNetworkId, destinationId: destinationId, payerNetworkId: &sourceNetworkId},
			usageOrigin:     &falseValue,
			originId:        destinationId,
			originNetworkId: sourceNetworkId,
		},
		{
			name:            "usage origin source",
			owner:           contractParticipantOwner{sourceNetworkId: sourceNetworkId, sourceId: sourceId, destinationNetworkId: destinationNetworkId, destinationId: destinationId, companionContractId: &companionId},
			usageOrigin:     &trueValue,
			originId:        sourceId,
			originNetworkId: destinationNetworkId,
		},
		{
			name:  "payer is not an endpoint",
			owner: contractParticipantOwner{sourceNetworkId: sourceNetworkId, sourceId: sourceId, destinationNetworkId: destinationNetworkId, destinationId: destinationId, payerNetworkId: &companionId},
			err:   true,
		},
	}
	for _, c := range cases {
		contractId := server.NewId()
		originId, originNetworkId, _, err := contractOrigin(contractId, c.owner, c.usageOrigin)
		if c.err {
			if err == nil {
				t.Fatalf("%s: expected an error", c.name)
			}
			_, _, participantsErr := contractParticipantsFromRows(contractId, c.owner, c.usageOrigin, nil)
			if participantsErr == nil {
				t.Fatalf("%s: participants accepted what origin refused", c.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %s", c.name, err)
		}
		connect.AssertEqual(t, originId, c.originId)
		connect.AssertEqual(t, originNetworkId, c.originNetworkId)

		// the billing participants exclude the origin and name the other endpoint
		participants, participantsOriginNetworkId, err := contractParticipantsFromRows(contractId, c.owner, c.usageOrigin, nil)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, participantsOriginNetworkId, originNetworkId)
		connect.AssertEqual(t, len(participants), 1)
		connect.AssertNotEqual(t, participants[0].ClientId, originId)
	}
}

// The setters name a limit even when it is nil, so code that builds args
// without JSON can clear one cap and leave the other unchanged.
func TestSetClientDataCapArgsSetters(t *testing.T) {
	args := &SetClientDataCapArgs{}
	connect.AssertEqual(t, args.monthlyByteLimitSet(), false)
	connect.AssertEqual(t, args.totalByteLimitSet(), false)

	args.SetMonthlyByteLimit(nil)
	connect.AssertEqual(t, args.monthlyByteLimitSet(), true)
	connect.AssertEqual(t, args.MonthlyByteLimit, (*ByteCount)(nil))
	connect.AssertEqual(t, args.totalByteLimitSet(), false)

	limit := ByteCount(5_000_000_000)
	args.SetTotalByteLimit(&limit)
	connect.AssertEqual(t, args.totalByteLimitSet(), true)
	connect.AssertEqual(t, *args.TotalByteLimit, limit)

	// merged against a current value: a named nil clears, an unnamed nil keeps
	current := ByteCount(10)
	connect.AssertEqual(t, mergeClientDataCapLimit(&current, args.monthlyByteLimitSet(), args.MonthlyByteLimit), (*ByteCount)(nil))
	untouched := &SetClientDataCapArgs{}
	connect.AssertEqual(t, *mergeClientDataCapLimit(&current, untouched.monthlyByteLimitSet(), untouched.MonthlyByteLimit), current)
}
