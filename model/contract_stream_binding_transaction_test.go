// A streamed contract's creation transaction does no Redis I/O for its
// stream: the binding reads the stream state before the transaction begins
// (contract_stream_binding.go). A statement tracer on the fixture pools and a
// hook on every Redis pool observe one creation request through its context,
// in send order, so the test sees each Redis command sent between the BEGIN
// and the COMMIT of the transaction that inserts the contract.
package model

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// Context key of the observer that a request's statements and Redis commands
// report to.
type creationIoObserverKey struct{}

// One Redis command: its name and its keys without their ids.
type creationIoCommand struct {
	description string
	// a stream membership, marking, pair or authority key
	streamState bool
	// an escrow admission reservation: every key is a reservation key, or a
	// keyless preflight of the admission's deadline pool
	admissionReservation bool
}

// One transaction of an observed request.
type creationIoTx struct {
	// the send order of its BEGIN
	beginOrder     int
	contractInsert bool
	// the Redis commands sent while it was open
	redisCommands []creationIoCommand
}

// Records, for requests whose context carries it, the transactions they open
// and the Redis commands they send, in send order. The tracer and the hook run
// on the sending goroutine. Safe for concurrent use.
type creationIoObserver struct {
	stateLock   sync.Mutex
	order       int
	openConnTxs map[*pgx.Conn]*creationIoTx
	// closed transactions that inserted a contract, since the last take
	creationTxs []*creationIoTx
	// the send orders of stream state commands sent while no transaction was
	// open, since the last take
	outsideStreamStateOrders []int
}

var creationContractInsertPattern = regexp.MustCompile(`(?i)insert\s+into\s+transfer_contract\s*\(`)

// Observes the statements and Redis commands of each context `observe`
// marks, until the returned stop runs. Start it before the test borrows a
// connection; stop it after every borrower returns. The hooks stay on the
// Redis clients, which the test environment resets after the test, and
// ignore unmarked contexts.
func startCreationIoObserver(t testing.TB, ctx context.Context) (observer *creationIoObserver, observe func(context.Context) context.Context, stop func()) {
	t.Helper()
	observer = &creationIoObserver{openConnTxs: map[*pgx.Conn]*creationIoTx{}}
	scope, err := server.NewTestPgQueryScope(ctx, observer)
	if err != nil {
		t.Fatal("the observer could not trace the fixture pools", err)
	}
	server.Redis(ctx, func(r server.RedisClient) {
		r.AddHook(observer)
	})
	server.RedisDoOnce(ctx, func(r server.RedisClient) {
		r.AddHook(observer)
	})
	// the deadline pool requires a bound; nothing here waits on it
	deadlineCtx, deadlineCancel := context.WithTimeout(ctx, time.Minute)
	defer deadlineCancel()
	err = server.RedisWithDeadline(deadlineCtx, func(r server.RedisClient) error {
		r.AddHook(observer)
		return nil
	})
	if err != nil {
		t.Fatal("the observer could not hook the deadline Redis pool", err)
	}
	err = server.RedisAuth(ctx, func(_ context.Context, r server.RedisClient) error {
		r.AddHook(observer)
		return nil
	})
	if err != nil {
		t.Fatal("the observer could not hook the authentication Redis pool", err)
	}
	observe = func(ctx context.Context) context.Context {
		return context.WithValue(ctx, creationIoObserverKey{}, observer)
	}
	stop = func() {
		server.Raise(scope.Close())
	}
	return
}

// The creation transactions closed, and the send orders of the stream state
// commands sent outside any transaction, since the last take.
func (self *creationIoObserver) take() (creationTxs []*creationIoTx, outsideStreamStateOrders []int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	creationTxs, outsideStreamStateOrders = self.creationTxs, self.outsideStreamStateOrders
	self.creationTxs, self.outsideStreamStateOrders = nil, nil
	return
}

func (self *creationIoObserver) observes(ctx context.Context) bool {
	return ctx.Value(creationIoObserverKey{}) == self
}

// Tracks the transaction boundaries and contract inserts of one statement.
func (self *creationIoObserver) statement(ctx context.Context, conn *pgx.Conn, sql string) {
	if !self.observes(ctx) {
		return
	}
	command := strings.ToLower(strings.TrimSpace(sql))
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.order += 1
	switch {
	case strings.HasPrefix(command, "begin"):
		self.openConnTxs[conn] = &creationIoTx{beginOrder: self.order}
	case command == "commit" || command == "rollback":
		if tx, ok := self.openConnTxs[conn]; ok {
			delete(self.openConnTxs, conn)
			if tx.contractInsert {
				self.creationTxs = append(self.creationTxs, tx)
			}
		}
	case creationContractInsertPattern.MatchString(sql):
		if tx, ok := self.openConnTxs[conn]; ok {
			tx.contractInsert = true
		}
	}
}

// Records Redis commands against every open transaction of the request.
// Keys are hash tagged, `{tag}suffix`. A command's description keeps its
// keys' suffixes, prefixed by the tag's kind for escrow and stream authority
// keys, never the ids or the binary stream keys inside the tags.
func (self *creationIoObserver) redisCommands(ctx context.Context, cmds []redis.Cmder) {
	if !self.observes(ctx) {
		return
	}
	describe := func(cmd redis.Cmder) creationIoCommand {
		parts := []string{cmd.Name()}
		keyCount := 0
		reservationKeyCount := 0
		streamState := false
		for _, arg := range cmd.Args()[1:] {
			key, ok := arg.(string)
			if !ok || !strings.HasPrefix(key, "{") {
				continue
			}
			tagEnd := strings.LastIndexByte(key, '}')
			if tagEnd < 0 {
				continue
			}
			keyCount += 1
			suffix := key[tagEnd+1:]
			switch {
			case strings.HasPrefix(key, "{escrow_"):
				reservationKeyCount += 1
				parts = append(parts, "escrow."+suffix)
			case strings.HasPrefix(key, "{sa_"):
				streamState = true
				parts = append(parts, "sa."+suffix)
			default:
				if strings.HasPrefix(suffix, "s2_") {
					streamState = true
				}
				parts = append(parts, suffix)
			}
		}
		return creationIoCommand{
			description:          strings.Join(parts, " "),
			streamState:          streamState,
			admissionReservation: reservationKeyCount == keyCount && (0 < keyCount || cmd.Name() == "ping"),
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, cmd := range cmds {
		self.order += 1
		command := describe(cmd)
		for _, tx := range self.openConnTxs {
			tx.redisCommands = append(tx.redisCommands, command)
		}
		if len(self.openConnTxs) == 0 && command.streamState {
			self.outsideStreamStateOrders = append(self.outsideStreamStateOrders, self.order)
		}
	}
}

func (self *creationIoObserver) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.statement(ctx, conn, data.SQL)
	return ctx
}

func (self *creationIoObserver) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// A batch's queued statements are sent in one round trip, in queue order.
func (self *creationIoObserver) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		self.statement(ctx, conn, query.SQL)
	}
	return ctx
}

func (self *creationIoObserver) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

func (self *creationIoObserver) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

func (self *creationIoObserver) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *creationIoObserver) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		self.redisCommands(ctx, []redis.Cmder{cmd})
		return next(ctx, cmd)
	}
}

func (self *creationIoObserver) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		self.redisCommands(ctx, cmds)
		return next(ctx, cmds)
	}
}

// Every creation owner of a streamed contract, escrow, companion, no-escrow
// and both zero escrow owners, with each binding: the transaction that
// inserts the contract sends no stream state command to Redis, and the
// stream state is read before that transaction begins. The no-escrow and zero
// escrow transactions send no Redis command at all; the escrow and companion
// transactions send only their admission reservations, which reserve the
// payer's balance, and the reservation pool's preflight pings. The insert
// still writes the stream the contract joins.
func TestContractCreationTransactionReadsNoStreamState(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		observer, observe, stopObserver := startCreationIoObserver(t, ctx)
		defer stopObserver()

		originNetwork, providerNetwork := server.NewId(), server.NewId()
		origin, first, egress := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: originNetwork, first: providerNetwork, egress: providerNetwork})
		addContractPayoutTestBalance(ctx, originNetwork, 1024)
		addContractPayoutTestBalance(ctx, providerNetwork, 1024)
		intermediaryIds := []server.Id{first}

		// exactly one creation transaction since the last check
		requireStreamStateReadBeforeTransaction := func(name string, admission bool) {
			t.Helper()
			creationTxs, outsideStreamStateOrders := observer.take()
			if len(creationTxs) != 1 {
				t.Fatal(name, "observed creation transactions", len(creationTxs))
			}
			creationTx := creationTxs[0]
			for _, command := range creationTx.redisCommands {
				if command.streamState {
					t.Fatal(name, "the creation transaction read the stream state from Redis:", command.description)
				}
			}
			for _, command := range creationTx.redisCommands {
				if !admission || !command.admissionReservation {
					t.Fatal(name, "the creation transaction sent a Redis command:", command.description)
				}
			}
			if len(outsideStreamStateOrders) == 0 || creationTx.beginOrder < outsideStreamStateOrders[0] {
				t.Fatal(name, "the stream state was not read before the creation transaction", outsideStreamStateOrders, creationTx.beginOrder)
			}
			if admission {
				descriptions := []string{}
				for _, command := range creationTx.redisCommands {
					descriptions = append(descriptions, command.description)
				}
				t.Log(name, "admission commands in the creation transaction:", descriptions)
			}
		}
		requireStreamWritten := func(name string, contractId server.Id, streamId server.Id) {
			t.Helper()
			if written := readContractStreamId(ctx, contractId); written == nil || *written != streamId {
				t.Fatal(name, "the insert did not write the stream id", written, streamId)
			}
		}

		// escrow: the first contract starts the stream with the id it bound.
		// The joins after commit are not observed.
		startCtx := WithAddStreamBinding(ctx, origin, egress, intermediaryIds)
		started, err := CreateTransferEscrow(observe(startCtx), originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		requireStreamStateReadBeforeTransaction("escrow start:", true)
		streamId := AddToStream(startCtx, started.ContractId, origin, egress, intermediaryIds)
		requireStreamWritten("escrow start:", started.ContractId, streamId)

		// no-escrow: a contract on the same path joins the live stream
		joinCtx := WithAddStreamBinding(ctx, origin, egress, intermediaryIds)
		joinedId, err := CreateContractNoEscrow(observe(joinCtx), originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		requireStreamStateReadBeforeTransaction("no-escrow join:", false)
		requireStreamWritten("no-escrow join:", joinedId, streamId)

		// zero escrow: the same path, without a reservation
		zeroCtx := WithAddStreamBinding(ctx, origin, egress, intermediaryIds)
		zero, err := CreateZeroEscrowContract(observe(zeroCtx), originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		requireStreamStateReadBeforeTransaction("zero escrow join:", false)
		requireStreamWritten("zero escrow join:", zero.ContractId, streamId)

		// companion: the reply rides its origin's stream, the pair's stream
		companionCtx := WithCompanionStreamBinding(ctx, egress, origin)
		companion, err := CreateCompanionTransferEscrow(observe(companionCtx), providerNetwork, egress, originNetwork, origin, 121, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		requireStreamStateReadBeforeTransaction("companion:", true)
		requireStreamWritten("companion:", companion.ContractId, streamId)

		// zero escrow companion
		zeroCompanionCtx := WithCompanionStreamBinding(ctx, egress, origin)
		zeroCompanion, err := CreateZeroEscrowCompanionContract(observe(zeroCompanionCtx), providerNetwork, egress, originNetwork, origin, 121, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		requireStreamStateReadBeforeTransaction("zero escrow companion:", false)
		requireStreamWritten("zero escrow companion:", zeroCompanion.ContractId, streamId)

		// no-escrow pair reply: a reply without the intermediary list joins
		// the pair's stream
		pairCtx := WithPairStreamBinding(ctx, egress, origin, false)
		replyId, err := CreateContractNoEscrow(observe(pairCtx), providerNetwork, egress, originNetwork, origin, 121)
		if err != nil {
			t.Fatal(err)
		}
		requireStreamStateReadBeforeTransaction("pair reply:", false)
		requireStreamWritten("pair reply:", replyId, streamId)
	})
}
