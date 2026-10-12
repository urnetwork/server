package controller

// Old clients retry a stale route on their own interval: a missing or inactive
// destination, or a return companion whose forward contract passed its
// deadline. Each identical request read PostgreSQL again to reach the same
// refusal. These tests drive the real CreateContract against the test database
// with a real origin notification owner and count the statements each request
// issues.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type routeRejectionStatementsKey struct{}

// Records the statements issued under one marked context, so other users of
// the fixture pools cannot change a count. Batches count each queued statement.
type routeRejectionStatements struct {
	stateLock  sync.Mutex
	statements []string
}

func (self *routeRejectionStatements) record(ctx context.Context, sql string) {
	if marker, _ := ctx.Value(routeRejectionStatementsKey{}).(*routeRejectionStatements); marker != self {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statements = append(self.statements, strings.Join(strings.Fields(sql), " "))
}

func (self *routeRejectionStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.record(ctx, data.SQL)
	return ctx
}

func (self *routeRejectionStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (self *routeRejectionStatements) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		self.record(ctx, query.SQL)
	}
	return ctx
}

func (self *routeRejectionStatements) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

func (self *routeRejectionStatements) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// Returns the statements recorded since the last take.
func (self *routeRejectionStatements) take() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	statements := self.statements
	self.statements = nil
	return statements
}

// The caller must defer the returned close inside its TestEnv callback.
func observeRouteRejectionStatements(t testing.TB, ctx context.Context) (context.Context, *routeRejectionStatements, func()) {
	t.Helper()
	statements := &routeRejectionStatements{}
	scope, err := server.NewTestPgQueryScope(ctx, statements)
	if err != nil {
		t.Fatal(err)
	}
	return context.WithValue(ctx, routeRejectionStatementsKey{}, statements), statements, func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}
}

// The owner an api router or connect exchange installs for its requests.
func newRouteRejectionOwnerContext(ctx context.Context) (context.Context, func()) {
	owner := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
	return model.WithContractOriginNotifications(ctx, owner), owner.Close
}

// Origin subscriptions this process acknowledged for the first time.
func originSubscriptionsAcknowledged(t testing.TB) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_contract_origin_notifications_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "event" && label.GetValue() == "subscribed" {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatal("the origin notification counter is not registered")
	return 0
}

// Starts the subscription bucket of a route's reverse pair and waits for its
// first acknowledgement. Until then an acknowledgement may close the fence of
// a missing-origin refusal and leave it unstored, so this is the barrier
// after which only an origin edge for the pair can. Call once per owner and
// bucket. The deadline only bounds a broken fixture.
func awaitRouteRejectionBucket(t testing.TB, ctx context.Context, sourceId server.Id, destinationId server.Id) {
	t.Helper()
	before := originSubscriptionsAcknowledged(t)
	watch := model.GetContractOriginNotifications(ctx).Watch(destinationId, sourceId)
	defer watch.Close()
	deadline := time.After(time.Minute)
	for {
		// the counter rises before the acknowledgement bumps the watch
		update := watch.Update()
		if before < originSubscriptionsAcknowledged(t) {
			return
		}
		select {
		case <-update:
		case <-deadline:
			t.Fatal("the origin subscription was not acknowledged")
		}
	}
}

// Requests one contract and returns its single decoded result.
func createRouteRejectionContract(t testing.TB, ctx context.Context, sourceId server.Id, destinationId server.Id, companion bool) *protocol.CreateContractResult {
	t.Helper()
	frames, err := CreateContract(ctx, sourceId, &protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		TransferByteCount: uint64(1024 * 1024),
		Companion:         companion,
	}, connect.DefaultContractManagerSettings())
	defer returnConnectControlFrames(frames)
	if err != nil || len(frames) != 1 {
		t.Fatalf("contract request failed: frames=%d err=%v", len(frames), err)
	}
	message, err := connect.FromFrame(frames[0])
	result, ok := message.(*protocol.CreateContractResult)
	if err != nil || !ok {
		t.Fatalf("contract result was not decoded: %v", err)
	}
	return result
}

func isRouteReliabilityRefusal(result *protocol.CreateContractResult) bool {
	return result.Contract == nil && result.Error != nil && *result.Error == protocol.ContractError_Reliability
}

// A provider replying to a retired window, or to an id that never existed,
// is refused by the destination lifecycle. The refusal is deterministic, so
// an identical repeat is answered without reading postgres.
func TestCreateContractRepeatedDestinationRefusalReadsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, closeOwner := newRouteRejectionOwnerContext(context.Background())
		defer closeOwner()
		sourceId, retiredId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), sourceId, "synthetic-provider", "synthetic")
		model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), retiredId, "synthetic-retired-window", "synthetic")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE network_client
				SET active = false, deactivate_time = clock_timestamp() AT TIME ZONE 'UTC'
				WHERE client_id = $1
			`, retiredId))
		})

		observed, statements, closeScope := observeRouteRejectionStatements(t, ctx)
		defer closeScope()
		for _, destination := range []struct {
			name string
			id   server.Id
		}{
			{name: "missing", id: server.NewId()},
			{name: "inactive", id: retiredId},
		} {
			for _, companion := range []bool{false, true} {
				first := createRouteRejectionContract(t, observed, sourceId, destination.id, companion)
				firstReads := statements.take()
				repeat := createRouteRejectionContract(t, observed, sourceId, destination.id, companion)
				repeatReads := statements.take()
				if !isRouteReliabilityRefusal(first) || !isRouteReliabilityRefusal(repeat) {
					t.Fatalf("%s destination (companion=%t) was not refused as an unreliable route: %+v, %+v", destination.name, companion, first, repeat)
				}
				if len(firstReads) == 0 {
					t.Fatalf("the first request to a %s destination read nothing", destination.name)
				}
				if len(repeatReads) != 0 {
					t.Fatalf("a repeated refusal of a %s destination (companion=%t) used %d statements, want 0 (the first used %d): %q", destination.name, companion, len(repeatReads), len(firstReads), repeatReads)
				}
			}
		}
	})
}

// A provider keeps returning traffic to a window whose forward contract
// passed its deadline, so no reverse origin exists: through the Stream
// fallback of a non-companion request, and as a requested companion. The
// refusal is answered without reading postgres while it repeats.
func TestCreateContractRepeatedMissingOriginReadsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, closeOwner := newRouteRejectionOwnerContext(context.Background())
		defer closeOwner()
		_, sourceId, _, destinationId := companionOriginTestSetup(ctx, t)
		// a window advertises only Stream, for its return traffic
		model.SetProvide(ctx, destinationId, map[model.ProvideMode][]byte{
			model.ProvideModeStream: []byte("synthetic-window-stream-key-0000"),
		})
		awaitRouteRejectionBucket(t, ctx, sourceId, destinationId)

		observed, statements, closeScope := observeRouteRejectionStatements(t, ctx)
		defer closeScope()
		for _, companion := range []bool{false, true} {
			first := createRouteRejectionContract(t, observed, sourceId, destinationId, companion)
			firstReads := statements.take()
			repeat := createRouteRejectionContract(t, observed, sourceId, destinationId, companion)
			repeatReads := statements.take()
			if !isRouteReliabilityRefusal(first) || !isRouteReliabilityRefusal(repeat) {
				t.Fatalf("a route without an origin (companion=%t) was not refused as unreliable: %+v, %+v", companion, first, repeat)
			}
			if len(firstReads) == 0 {
				t.Fatal("the first request without an origin read nothing")
			}
			if len(repeatReads) != 0 {
				t.Fatalf("a repeated missing-origin refusal (companion=%t) used %d statements, want 0 (the first used %d): %q", companion, len(repeatReads), len(firstReads), repeatReads)
			}
		}
	})
}

// A destination retired between the lifecycle read and the write boundary is
// refused by the write boundary's own active read. That refusal is kept too,
// so an identical repeat reaches neither read. The Stream fallback's origin
// lookup still found the window's forward contract.
func TestCreateContractRecordsDestinationRetiredAtTheWriteBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, closeOwner := newRouteRejectionOwnerContext(context.Background())
		defer closeOwner()
		sourceId, destinationId := newCompanionFallbackPair(t, ctx)
		previousPairNetworks, previousOriginExists := findActiveClientPairNetworks, companionOriginExists
		defer func() {
			findActiveClientPairNetworks, companionOriginExists = previousPairNetworks, previousOriginExists
		}()
		companionOriginExists = func(context.Context, server.Id, server.Id, time.Duration) bool {
			return true
		}
		pairReads := 0
		findActiveClientPairNetworks = func(ctx context.Context, sourceId server.Id, destinationId server.Id) (*server.Id, *server.Id) {
			pairReads += 1
			sourceNetworkId, _ := previousPairNetworks(ctx, sourceId, destinationId)
			// the destination was deactivated after the lifecycle read
			return sourceNetworkId, nil
		}

		observed, statements, closeScope := observeRouteRejectionStatements(t, ctx)
		defer closeScope()
		first := createRouteRejectionContract(t, observed, sourceId, destinationId, false)
		firstReads, firstPairReads := statements.take(), pairReads
		repeat := createRouteRejectionContract(t, observed, sourceId, destinationId, false)
		repeatReads, repeatPairReads := statements.take(), pairReads-firstPairReads
		if !isRouteReliabilityRefusal(first) || !isRouteReliabilityRefusal(repeat) {
			t.Fatalf("a destination retired at the write boundary was not refused as an unreliable route: %+v, %+v", first, repeat)
		}
		if firstPairReads != 1 || len(firstReads) == 0 {
			t.Fatalf("the first request made %d write boundary reads and %d statements, want 1 and some", firstPairReads, len(firstReads))
		}
		if repeatPairReads != 0 || len(repeatReads) != 0 {
			t.Fatalf("a repeated refusal at the write boundary made %d write boundary reads and used %d statements, want 0 and 0 (the first used %d): %q", repeatPairReads, len(repeatReads), len(firstReads), repeatReads)
		}
	})
}

// The window creates a new forward contract after its old one expired. The
// provider's next return request must ride it, not replay the refusal.
func TestCreateContractRouteRefusalEndsWhenItsOriginCommits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, closeOwner := newRouteRejectionOwnerContext(context.Background())
		defer closeOwner()
		sourceNetworkId, sourceId, destinationNetworkId, destinationId := companionOriginTestSetup(ctx, t)
		model.SetProvide(ctx, destinationId, map[model.ProvideMode][]byte{
			model.ProvideModeStream: []byte("synthetic-window-stream-key-0001"),
		})
		awaitRouteRejectionBucket(t, ctx, sourceId, destinationId)
		if result := createRouteRejectionContract(t, ctx, sourceId, destinationId, false); !isRouteReliabilityRefusal(result) {
			t.Fatalf("a route without an origin was not refused: %+v", result)
		}
		if _, ok := model.GetContractOriginNotifications(ctx).RouteRejection(sourceId, destinationId, false); !ok {
			t.Fatal("the missing-origin refusal was not recorded")
		}

		// the window's new forward contract, through this owner
		origin, err := model.CreateTransferEscrow(ctx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, model.ByteCount(1024*1024))
		if err != nil || origin == nil {
			t.Fatalf("origin creation failed: %v", err)
		}
		result := createRouteRejectionContract(t, ctx, sourceId, destinationId, false)
		if result.Contract == nil {
			t.Fatalf("the return request after its origin committed was refused: %+v", result)
		}
	})
}

// Only the exact deterministic verdicts are recorded. A missing origin joined
// with another failure, an operational failure, and a created contract all
// repeat their authoritative work on an identical request. Each case owns an
// acknowledged bucket, so a verdict that was wrongly recorded would be kept.
// The Stream fallback's origin lookup finds an origin, so the write path
// decides each case.
func TestCreateContractRecordsOnlyExactRouteRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		previousCompanion, previousOriginExists := createCompanionTransferEscrow, companionOriginExists
		defer func() { createCompanionTransferEscrow, companionOriginExists = previousCompanion, previousOriginExists }()
		companionOriginExists = func(context.Context, server.Id, server.Id, time.Duration) bool {
			return true
		}
		originId := server.NewId()
		for _, outcome := range []struct {
			name   string
			escrow *model.TransferEscrow
			err    error
		}{
			{name: "joined missing origin", err: errors.Join(model.ErrMissingCompanionOrigin, errors.New("synthetic cleanup failure"))},
			{name: "operational failure", err: errors.New("synthetic operational failure")},
			{name: "created contract", escrow: &model.TransferEscrow{
				CompanionContractId: &originId,
				ExpirationTime:      time.Now().Add(time.Hour),
				TransferByteCount:   model.ByteCount(1024 * 1024),
			}},
		} {
			ctx, closeOwner := newRouteRejectionOwnerContext(context.Background())
			defer closeOwner()
			sourceId, destinationId := newCompanionFallbackPair(t, ctx)
			awaitRouteRejectionBucket(t, ctx, sourceId, destinationId)
			creations := 0
			createCompanionTransferEscrow = func(context.Context, server.Id, server.Id, server.Id, server.Id, model.ByteCount, time.Duration) (*model.TransferEscrow, error) {
				creations += 1
				if outcome.escrow == nil {
					return nil, outcome.err
				}
				escrow := *outcome.escrow
				escrow.ContractId = server.NewId()
				return &escrow, nil
			}
			for range 2 {
				createRouteRejectionContract(t, ctx, sourceId, destinationId, false)
			}
			if creations != 2 {
				t.Fatalf("%s: an identical request made %d of 2 authoritative attempts", outcome.name, creations)
			}
			if _, ok := model.GetContractOriginNotifications(ctx).RouteRejection(sourceId, destinationId, false); ok {
				t.Fatalf("%s was recorded as a route refusal", outcome.name)
			}
		}
	})
}

// The origin commits while the missing-origin read is in flight: the read found
// nothing, and the commit's edge arrives before the refusal is returned. The
// refusal is reported but not recorded, because the edge closed the fence its
// read armed, so the next identical request rides the new origin.
func TestCreateContractMissingOriginAcrossItsCommitIsNotRecorded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, closeOwner := newRouteRejectionOwnerContext(context.Background())
		defer closeOwner()
		sourceNetworkId, sourceId, destinationNetworkId, destinationId := companionOriginTestSetup(ctx, t)
		model.SetProvide(ctx, destinationId, map[model.ProvideMode][]byte{
			model.ProvideModeStream: []byte("synthetic-window-stream-key-0002"),
		})
		awaitRouteRejectionBucket(t, ctx, sourceId, destinationId)

		// the Stream fallback's origin lookup is the read that decides
		previousOriginExists := companionOriginExists
		defer func() { companionOriginExists = previousOriginExists }()
		companionOriginExists = func(ctx context.Context, _ server.Id, _ server.Id, _ time.Duration) bool {
			watch := model.GetContractOriginNotifications(ctx).Watch(destinationId, sourceId)
			defer watch.Close()
			edge := watch.Update()
			// the window's new forward contract, through this owner
			origin, err := model.CreateTransferEscrow(ctx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, model.ByteCount(1024*1024))
			if err != nil || origin == nil {
				t.Errorf("origin creation failed: %v", err)
			}
			// the deadline only bounds a broken fixture
			select {
			case <-edge:
			case <-time.After(time.Minute):
				t.Error("the origin edge did not arrive")
			}
			// the read's snapshot predates the commit
			return false
		}
		if result := createRouteRejectionContract(t, ctx, sourceId, destinationId, false); !isRouteReliabilityRefusal(result) {
			t.Fatalf("the refusal read before its origin committed was not reported: %+v", result)
		}
		if _, ok := model.GetContractOriginNotifications(ctx).RouteRejection(sourceId, destinationId, false); ok {
			t.Fatal("a refusal read across its origin's commit was recorded")
		}

		companionOriginExists = previousOriginExists
		if result := createRouteRejectionContract(t, ctx, sourceId, destinationId, false); result.Contract == nil {
			t.Fatalf("the request after its origin committed was refused: %+v", result)
		}
	})
}

// An old provider keeps replying to a retired prober window every 5s, and the
// load balancer spreads its retries over several api processes, so a process
// sees the route again only after the short ring has expired the refusal.
// The ring is disabled here, which is that retry deterministically. A refusal
// of an inactive destination row is terminal and still answers it without a
// read; a missing destination row is not terminal and reads again.
func TestCreateContractInactiveDestinationRefusalOutlivesTheRing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := model.DefaultContractOriginNotificationSettings()
		settings.RouteRejectionTimeout = 0
		owner := model.NewContractOriginNotifications(context.Background(), settings)
		defer owner.Close()
		ctx := model.WithContractOriginNotifications(context.Background(), owner)
		sourceId, retiredId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), sourceId, "synthetic-provider", "synthetic")
		model.Testing_CreateDevice(ctx, server.NewId(), server.NewId(), retiredId, "synthetic-retired-window", "synthetic")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE network_client
				SET active = false, deactivate_time = clock_timestamp() AT TIME ZONE 'UTC'
				WHERE client_id = $1
			`, retiredId))
		})

		observed, statements, closeScope := observeRouteRejectionStatements(t, ctx)
		defer closeScope()
		for _, companion := range []bool{false, true} {
			first := createRouteRejectionContract(t, observed, sourceId, retiredId, companion)
			firstReads := statements.take()
			retry := createRouteRejectionContract(t, observed, sourceId, retiredId, companion)
			retryReads := statements.take()
			if !isRouteReliabilityRefusal(first) || !isRouteReliabilityRefusal(retry) {
				t.Fatalf("inactive destination (companion=%t) was not refused as an unreliable route: %+v, %+v", companion, first, retry)
			}
			if len(firstReads) == 0 {
				t.Fatal("the first request to an inactive destination read nothing")
			}
			if len(retryReads) != 0 {
				t.Fatalf("a retry to an inactive destination after the ring timeout (companion=%t) used %d statements, want 0 (the first used %d): %q", companion, len(retryReads), len(firstReads), retryReads)
			}
		}

		missingId := server.NewId()
		createRouteRejectionContract(t, observed, sourceId, missingId, false)
		statements.take()
		if retry := createRouteRejectionContract(t, observed, sourceId, missingId, false); !isRouteReliabilityRefusal(retry) {
			t.Fatalf("missing destination was not refused as an unreliable route: %+v", retry)
		}
		if retryReads := statements.take(); len(retryReads) == 0 {
			t.Fatal("a missing destination row was kept past the ring timeout")
		}
	})
}

// An old provider returns traffic to a window whose forward contract passed
// its deadline, as a non-companion request the destination settles as a
// Stream companion. Its one origin lookup decides the refusal before the
// destination's metadata and the endpoint lifecycle reads, which only a
// contract needs.
func TestCreateContractStreamFallbackDecidesMissingOriginBeforeContractReads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, closeOwner := newRouteRejectionOwnerContext(context.Background())
		defer closeOwner()
		sourceId, destinationId := newCompanionFallbackPair(t, ctx)
		awaitRouteRejectionBucket(t, ctx, sourceId, destinationId)
		previousSecret := getProvideSecretKey
		defer func() { getProvideSecretKey = previousSecret }()
		secretReads := 0
		getProvideSecretKey = func(ctx context.Context, clientId server.Id, provideMode model.ProvideMode) ([]byte, error) {
			secretReads += 1
			return previousSecret(ctx, clientId, provideMode)
		}

		observed, statements, closeScope := observeRouteRejectionStatements(t, ctx)
		defer closeScope()
		result := createRouteRejectionContract(t, observed, sourceId, destinationId, false)
		reads := statements.take()
		if !isRouteReliabilityRefusal(result) {
			t.Fatalf("a Stream fallback without an origin was not refused as an unreliable route: %+v", result)
		}
		originReads := 0
		for _, statement := range reads {
			switch {
			case strings.Contains(statement, "tls_certificate_pem"):
				t.Fatalf("the refusal read the destination's contract metadata: %q", reads)
			case strings.Contains(statement, "WHERE client_id = $1 AND active), (SELECT network_id"):
				t.Fatalf("the refusal read the endpoint lifecycle for a contract it never creates: %q", reads)
			case strings.Contains(statement, "SELECT EXISTS ( SELECT 1 FROM transfer_contract"):
				originReads += 1
			}
		}
		if originReads != 1 {
			t.Fatalf("the refusal made %d origin reads, want 1: %q", originReads, reads)
		}
		if secretReads != 0 {
			t.Fatalf("the refusal read the provide secret %d times, want 0", secretReads)
		}
		if _, ok := model.GetContractOriginNotifications(ctx).RouteRejection(sourceId, destinationId, false); !ok {
			t.Fatal("the missing-origin refusal was not recorded")
		}
	})
}
