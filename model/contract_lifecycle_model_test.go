package model

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Cross-host wall-clock skew is not portable to induce in a unit process. The
// behavioral lock tests below prove serialization; this source-level guard
// independently proves that every serialized writer takes its timestamp from
// PostgreSQL after the lock marker instead of reintroducing an application
// clock argument.
func TestContractLifecycleWritesUsePrimaryDatabaseClockAfterLocks(t *testing.T) {
	type parsedSource struct {
		filename string
		data     []byte
		fileSet  *token.FileSet
		file     *ast.File
	}
	sources := []parsedSource{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, entry.Name(), data, 0)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, parsedSource{
			filename: entry.Name(),
			data:     data,
			fileSet:  fileSet,
			file:     file,
		})
	}

	functionSource := func(filename string, functionName string) string {
		for _, source := range sources {
			if source.filename != filename {
				continue
			}
			for _, declaration := range source.file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Name.Name != functionName {
					continue
				}
				start := source.fileSet.Position(function.Pos()).Offset
				end := source.fileSet.Position(function.End()).Offset
				return string(source.data[start:end])
			}
		}
		t.Fatalf("%s: function %s not found", filename, functionName)
		return ""
	}

	boundaries := []struct {
		filename     string
		functionName string
		lockMarker   string
		writeMarker  string
	}{
		{filename: "subscription_model.go", functionName: "createTransferEscrowInTx", lockMarker: "if err := lockActiveContractClientsInTx(", writeMarker: "INSERT INTO transfer_contract"},
		{filename: "subscription_model.go", functionName: "createContractNoEscrowInTx", lockMarker: "if err := lockActiveContractClientsInTx(", writeMarker: "INSERT INTO transfer_contract"},
		{filename: "network_client_model.go", functionName: "deactivateNetworkClientsInTx", lockMarker: "/* network_client_deactivation_write_boundary */", writeMarker: "WITH lifecycle_clock AS MATERIALIZED"},
		{filename: "network_client_model.go", functionName: "RemoveDisconnectedNetworkClients", lockMarker: "FOR UPDATE OF network_client", writeMarker: "WITH lifecycle_clock AS MATERIALIZED"},
	}

	for _, boundary := range boundaries {
		source := functionSource(boundary.filename, boundary.functionName)
		for markerName, marker := range map[string]string{
			"lock":  boundary.lockMarker,
			"write": boundary.writeMarker,
			"clock": "clock_timestamp() AT TIME ZONE 'UTC'",
		} {
			if count := strings.Count(source, marker); count != 1 {
				t.Errorf("%s: %s marker count = %d, want 1", boundary.functionName, markerName, count)
			}
		}
		lockIndex := strings.Index(source, boundary.lockMarker)
		writeIndex := strings.Index(source, boundary.writeMarker)
		clockIndex := strings.Index(source, "clock_timestamp() AT TIME ZONE 'UTC'")
		if lockIndex < 0 || writeIndex <= lockIndex || clockIndex <= writeIndex {
			t.Errorf(
				"%s: want lock (%d) before write (%d) before database clock (%d)",
				boundary.functionName,
				lockIndex,
				writeIndex,
				clockIndex,
			)
		}
	}

	contractWriters := []string{}
	deactivationWriters := []string{}
	for _, source := range sources {
		ast.Inspect(source.file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatalf("%s: parse string literal: %v", source.filename, err)
			}
			normalized := strings.ToUpper(strings.Join(strings.Fields(value), " "))
			if strings.Contains(normalized, "INSERT INTO TRANSFER_CONTRACT") {
				contractWriters = append(contractWriters, normalized)
			}
			if strings.Contains(normalized, "UPDATE NETWORK_CLIENT") && strings.Contains(normalized, "ACTIVE = FALSE") {
				deactivationWriters = append(deactivationWriters, normalized)
			}
			return true
		})
	}
	if len(contractWriters) != 2 {
		t.Fatalf("production transfer_contract INSERT writers = %d, want exactly 2 reviewed writers", len(contractWriters))
	}
	for writerIndex, writer := range contractWriters {
		if !strings.Contains(writer, "CLOCK_TIMESTAMP() AT TIME ZONE 'UTC'") {
			t.Errorf("transfer_contract writer %d does not use the primary database clock", writerIndex)
		}
	}
	if len(deactivationWriters) != 2 {
		t.Fatalf("production network_client active=false writers = %d, want exactly 2 reviewed writers", len(deactivationWriters))
	}
	for writerIndex, writer := range deactivationWriters {
		for _, want := range []string{
			"LIFECYCLE_CLOCK AS MATERIALIZED",
			"CLOCK_TIMESTAMP() AT TIME ZONE 'UTC'",
			"DEACTIVATE_TIME = LIFECYCLE_CLOCK.DEACTIVATE_TIME",
		} {
			if !strings.Contains(writer, want) {
				t.Errorf("network_client deactivation writer %d missing %q", writerIndex, want)
			}
		}
	}
}

type contractLifecycleTestResult struct {
	contractId server.Id
	escrow     *TransferEscrow
	rowCount   int64
	err        error
}

func insertContractLifecycleTestClients(
	t testing.TB,
	ctx context.Context,
	clients map[server.Id]server.Id,
) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for clientId, networkId := range clients {
			_, err := tx.Exec(
				ctx,
				`
					INSERT INTO network_client (
						client_id,
						network_id,
						active,
						create_time,
						auth_time
					)
					VALUES ($1, $2, true, $3, $3)
				`,
				clientId,
				networkId,
				server.NowUtc(),
			)
			if err != nil {
				t.Fatal(err)
			}
		}
	})
}

func acquireContractLifecycleTestConnection(
	t testing.TB,
	ctx context.Context,
) server.PgConn {
	t.Helper()
	conn, err := server.AcquireMaintenanceDbConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET default_transaction_read_only = off`); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	return conn
}

func contractLifecycleTestBackendPid(
	t testing.TB,
	ctx context.Context,
	tx server.PgTx,
) int32 {
	t.Helper()
	var backendPid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPid); err != nil {
		t.Fatal(err)
	}
	return backendPid
}

func requireContractLifecycleBlockedBy(
	t testing.TB,
	ctx context.Context,
	observer server.PgTx,
	blockerPid int32,
) int32 {
	t.Helper()
	for {
		var blockedPid int32
		// pg_stat_activity can retain one statistics snapshot for this open
		// transaction and hides another role's query text without elevated
		// privileges. pg_locks exposes the synchronization fact this test needs
		// without either dependency.
		err := observer.QueryRow(
			ctx,
			`
				SELECT COALESCE(min(pid), 0)
				FROM pg_locks
				WHERE
					NOT granted AND
					$1::int = ANY(pg_blocking_pids(pid))
			`,
			blockerPid,
		).Scan(&blockedPid)
		if err != nil {
			t.Fatal(err)
		}
		if blockedPid != 0 {
			return blockedPid
		}
		select {
		case <-ctx.Done():
			t.Fatalf("lifecycle lock was not observed: %v", ctx.Err())
		default:
			runtime.Gosched()
		}
	}
}

func contractLifecycleTestCount(
	t testing.TB,
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
) int64 {
	t.Helper()
	var count int64
	server.Db(ctx, func(conn server.PgConn) {
		if err := conn.QueryRow(
			ctx,
			`
				SELECT count(*)
				FROM transfer_contract
				WHERE source_id = $1 AND destination_id = $2
			`,
			sourceId,
			destinationId,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
	})
	return count
}

func TestCreateContractNoEscrowSerializesWithDestinationDeactivation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
			sourceId:      sourceNetworkId,
			destinationId: destinationNetworkId,
		})

		deactivationConn := acquireContractLifecycleTestConnection(t, ctx)
		deactivationTx, err := deactivationConn.Begin(ctx)
		if err != nil {
			deactivationConn.Release()
			t.Fatal(err)
		}
		deactivationFinished := false
		defer func() {
			if !deactivationFinished {
				_ = deactivationTx.Rollback(context.Background())
			}
			deactivationConn.Release()
		}()
		if _, err := deactivationTx.Exec(
			ctx,
			`
				UPDATE network_client
				SET active = false, deactivate_time = $2
				WHERE client_id = $1
			`,
			destinationId,
			server.NowUtc(),
		); err != nil {
			t.Fatal(err)
		}
		deactivationPid := contractLifecycleTestBackendPid(t, ctx, deactivationTx)

		contractResult := make(chan contractLifecycleTestResult, 1)
		go func() {
			result := contractLifecycleTestResult{}
			defer func() {
				if value := recover(); value != nil {
					result.err = fmt.Errorf("CreateContractNoEscrow panic: %v", value)
				}
				contractResult <- result
			}()
			result.contractId, result.err = CreateContractNoEscrow(
				ctx,
				sourceNetworkId,
				sourceId,
				destinationNetworkId,
				destinationId,
				1024,
			)
		}()

		blockedPid := requireContractLifecycleBlockedBy(
			t,
			ctx,
			deactivationTx,
			deactivationPid,
		)
		if blockedPid == deactivationPid {
			t.Fatal("deactivation backend was reported as its own waiter")
		}
		select {
		case result := <-contractResult:
			t.Fatalf("contract creation crossed the lifecycle lock: id=%s err=%v", result.contractId, result.err)
		default:
		}

		if err := deactivationTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		deactivationFinished = true

		var result contractLifecycleTestResult
		select {
		case result = <-contractResult:
		case <-ctx.Done():
			t.Fatalf("contract creation did not finish: %v", ctx.Err())
		}
		if !errors.Is(result.err, ErrContractDestinationInactive) {
			t.Fatalf("CreateContractNoEscrow() error = %v, want destination inactive", result.err)
		}
		if result.contractId != (server.Id{}) {
			t.Fatalf("inactive destination returned contract id %s", result.contractId)
		}
		if count := contractLifecycleTestCount(t, ctx, sourceId, destinationId); count != 0 {
			t.Fatalf("inactive destination contract count = %d, want 0", count)
		}
	})
}

func TestPlainContractInsertDoesNotSerializeWithDeactivation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
			sourceId:      sourceNetworkId,
			destinationId: destinationNetworkId,
		})

		deactivationConn := acquireContractLifecycleTestConnection(t, ctx)
		defer deactivationConn.Release()
		deactivationTx, err := deactivationConn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		deactivationFinished := false
		defer func() {
			if !deactivationFinished {
				_ = deactivationTx.Rollback(context.Background())
			}
		}()
		if _, err := deactivationTx.Exec(
			ctx,
			`UPDATE network_client SET active = false, deactivate_time = $2 WHERE client_id = $1`,
			destinationId,
			server.NowUtc(),
		); err != nil {
			t.Fatal(err)
		}

		plainConn := acquireContractLifecycleTestConnection(t, ctx)
		defer plainConn.Release()
		plainTx, err := plainConn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		plainFinished := false
		defer func() {
			if !plainFinished {
				_ = plainTx.Rollback(context.Background())
			}
		}()
		contractId := server.NewId()
		if _, err := plainTx.Exec(
			ctx,
			`
				INSERT INTO transfer_contract (
					contract_id,
					source_network_id,
					source_id,
					destination_network_id,
					destination_id,
					transfer_byte_count,
					create_time
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
			`,
			contractId,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			1024,
			server.NowUtc(),
		); err != nil {
			t.Fatalf("plain insert did not reproduce the old race: %v", err)
		}
		var inserted bool
		if err := plainTx.QueryRow(
			ctx,
			`SELECT EXISTS (SELECT 1 FROM transfer_contract WHERE contract_id = $1)`,
			contractId,
		).Scan(&inserted); err != nil {
			t.Fatal(err)
		}
		if !inserted {
			t.Fatal("plain insert returned without creating its contract row")
		}
		if err := plainTx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		plainFinished = true
		if err := deactivationTx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		deactivationFinished = true
	})
}

func TestContractLifecycleLockValidatesNetworkMappings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
			sourceId:      sourceNetworkId,
			destinationId: destinationNetworkId,
		})

		_, err := CreateContractNoEscrow(
			ctx,
			server.NewId(),
			sourceId,
			destinationNetworkId,
			destinationId,
			1024,
		)
		if !errors.Is(err, ErrActiveClientNotFound) {
			t.Fatalf("source network mismatch error = %v, want inactive source", err)
		}

		_, err = CreateContractNoEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			server.NewId(),
			destinationId,
			1024,
		)
		if !errors.Is(err, ErrContractDestinationInactive) {
			t.Fatalf("destination network mismatch error = %v, want inactive destination", err)
		}
		if count := contractLifecycleTestCount(t, ctx, sourceId, destinationId); count != 0 {
			t.Fatalf("network-mismatched contract count = %d, want 0", count)
		}
	})
}

func TestCreateTransferEscrowSerializesWithDestinationDeactivation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		now := server.NowUtc()
		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
			sourceId:      sourceNetworkId,
			destinationId: destinationNetworkId,
		})
		transferBalance := &TransferBalance{
			NetworkId:             sourceNetworkId,
			StartTime:             now.Add(-time.Hour),
			EndTime:               now.Add(time.Hour),
			StartBalanceByteCount: 4096,
			BalanceByteCount:      4096,
			PurchaseToken:         "synthetic-" + server.NewId().String(),
		}
		AddTransferBalance(ctx, transferBalance)

		deactivationConn := acquireContractLifecycleTestConnection(t, ctx)
		deactivationTx, err := deactivationConn.Begin(ctx)
		if err != nil {
			deactivationConn.Release()
			t.Fatal(err)
		}
		deactivationFinished := false
		defer func() {
			if !deactivationFinished {
				_ = deactivationTx.Rollback(context.Background())
			}
			deactivationConn.Release()
		}()
		if _, err := deactivationTx.Exec(
			ctx,
			`
				UPDATE network_client
				SET active = false, deactivate_time = $2
				WHERE client_id = $1
			`,
			destinationId,
			server.NowUtc(),
		); err != nil {
			t.Fatal(err)
		}
		deactivationPid := contractLifecycleTestBackendPid(t, ctx, deactivationTx)

		escrowResult := make(chan contractLifecycleTestResult, 1)
		go func() {
			result := contractLifecycleTestResult{}
			defer func() {
				if value := recover(); value != nil {
					result.err = fmt.Errorf("CreateTransferEscrow panic: %v", value)
				}
				escrowResult <- result
			}()
			result.escrow, result.err = CreateTransferEscrow(
				ctx,
				sourceNetworkId,
				sourceId,
				destinationNetworkId,
				destinationId,
				1024,
			)
		}()

		blockedPid := requireContractLifecycleBlockedBy(
			t,
			ctx,
			deactivationTx,
			deactivationPid,
		)
		if blockedPid == deactivationPid {
			t.Fatal("deactivation backend was reported as its own waiter")
		}
		select {
		case result := <-escrowResult:
			t.Fatalf("escrow creation crossed the lifecycle lock: escrow=%+v err=%v", result.escrow, result.err)
		default:
		}

		if err := deactivationTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		deactivationFinished = true

		var result contractLifecycleTestResult
		select {
		case result = <-escrowResult:
		case <-ctx.Done():
			t.Fatalf("escrow creation did not finish: %v", ctx.Err())
		}
		if !errors.Is(result.err, ErrContractDestinationInactive) {
			t.Fatalf("CreateTransferEscrow() error = %v, want destination inactive", result.err)
		}
		if result.escrow != nil {
			t.Fatalf("inactive destination returned escrow %+v", result.escrow)
		}
		if count := contractLifecycleTestCount(t, ctx, sourceId, destinationId); count != 0 {
			t.Fatalf("inactive destination contract count = %d, want 0", count)
		}
		var escrowCount int64
		server.Db(ctx, func(conn server.PgConn) {
			if err := conn.QueryRow(
				ctx,
				`SELECT count(*) FROM transfer_escrow WHERE balance_id = $1`,
				transferBalance.BalanceId,
			).Scan(&escrowCount); err != nil {
				t.Fatal(err)
			}
		})
		if escrowCount != 0 {
			t.Fatalf("inactive destination escrow row count = %d, want 0", escrowCount)
		}
		if netEscrow := Testing_NetEscrowByteCount(ctx, transferBalance.BalanceId); netEscrow != 0 {
			t.Fatalf("inactive destination net escrow = %d, want 0", netEscrow)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			rowCount, err := deactivateNetworkClientsInTx(
				ctx,
				tx,
				[]server.Id{sourceId},
				sourceNetworkId,
			)
			if err != nil {
				t.Fatal(err)
			}
			if rowCount != 1 {
				t.Fatalf("deactivated source rows = %d, want 1", rowCount)
			}
		})
		_, err = CreateContractNoEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			1024,
		)
		if !errors.Is(err, ErrActiveClientNotFound) {
			t.Fatalf("CreateContractNoEscrow() error = %v, want inactive source", err)
		}
	})
}

func TestDeactivationTimestampFollowsLifecycleLock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		networkId := server.NewId()
		clientIds := []server.Id{server.NewId(), server.NewId()}
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
			clientIds[0]: networkId,
			clientIds[1]: networkId,
		})
		slices.SortFunc(clientIds, func(a server.Id, b server.Id) int {
			return a.Cmp(b)
		})

		readerConn := acquireContractLifecycleTestConnection(t, ctx)
		defer readerConn.Release()
		readerTx, err := readerConn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		readerFinished := false
		defer func() {
			if !readerFinished {
				_ = readerTx.Rollback(context.Background())
			}
		}()
		result, err := readerTx.Query(
			ctx,
			`
				SELECT client_id
				FROM network_client
				WHERE client_id = ANY($1)
				ORDER BY client_id
				FOR SHARE
			`,
			clientIds,
		)
		if err != nil {
			t.Fatal(err)
		}
		lockedCount := 0
		for result.Next() {
			var clientId server.Id
			if err := result.Scan(&clientId); err != nil {
				t.Fatal(err)
			}
			lockedCount++
		}
		if err := result.Err(); err != nil {
			t.Fatal(err)
		}
		result.Close()
		if lockedCount != len(clientIds) {
			t.Fatalf("reader locked %d clients, want %d", lockedCount, len(clientIds))
		}
		readerPid := contractLifecycleTestBackendPid(t, ctx, readerTx)

		deactivationConn := acquireContractLifecycleTestConnection(t, ctx)
		defer deactivationConn.Release()
		deactivationTx, err := deactivationConn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		deactivationPid := contractLifecycleTestBackendPid(t, ctx, deactivationTx)
		deactivationResult := make(chan contractLifecycleTestResult, 1)
		go func() {
			rowCount, deactivateErr := deactivateNetworkClientsInTx(
				ctx,
				deactivationTx,
				[]server.Id{clientIds[1], clientIds[0]},
				networkId,
			)
			if deactivateErr == nil {
				deactivateErr = deactivationTx.Commit(ctx)
			} else {
				_ = deactivationTx.Rollback(context.Background())
			}
			deactivationResult <- contractLifecycleTestResult{
				rowCount: rowCount,
				err:      deactivateErr,
			}
		}()

		blockedPid := requireContractLifecycleBlockedBy(
			t,
			ctx,
			readerTx,
			readerPid,
		)
		if blockedPid != deactivationPid {
			t.Fatalf("blocked backend = %d, want deactivation backend %d", blockedPid, deactivationPid)
		}
		select {
		case result := <-deactivationResult:
			t.Fatalf("deactivation crossed the lifecycle lock: rows=%d err=%v", result.rowCount, result.err)
		default:
		}

		var releaseTime time.Time
		if err := readerTx.QueryRow(
			ctx,
			`SELECT clock_timestamp() AT TIME ZONE 'UTC'`,
		).Scan(&releaseTime); err != nil {
			t.Fatal(err)
		}
		if err := readerTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		readerFinished = true

		var deactivated contractLifecycleTestResult
		select {
		case deactivated = <-deactivationResult:
		case <-ctx.Done():
			t.Fatalf("deactivation did not finish: %v", ctx.Err())
		}
		if deactivated.err != nil {
			t.Fatal(deactivated.err)
		}
		if deactivated.rowCount != int64(len(clientIds)) {
			t.Fatalf("deactivated rows = %d, want %d", deactivated.rowCount, len(clientIds))
		}

		deactivateTimes := []time.Time{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT deactivate_time
					FROM network_client
					WHERE client_id = ANY($1)
					ORDER BY client_id
				`,
				clientIds,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer result.Close()
			for result.Next() {
				var deactivateTime time.Time
				if err := result.Scan(&deactivateTime); err != nil {
					t.Fatal(err)
				}
				deactivateTimes = append(deactivateTimes, deactivateTime)
			}
			if err := result.Err(); err != nil {
				t.Fatal(err)
			}
		})
		if len(deactivateTimes) != len(clientIds) {
			t.Fatalf("deactivation timestamps = %d, want %d", len(deactivateTimes), len(clientIds))
		}
		for _, deactivateTime := range deactivateTimes {
			if deactivateTime.Before(releaseTime) {
				t.Fatalf("deactivation time %s predates lock release %s", deactivateTime, releaseTime)
			}
		}
		if !deactivateTimes[0].Equal(deactivateTimes[1]) {
			t.Fatalf("batch deactivation timestamps differ: %s and %s", deactivateTimes[0], deactivateTimes[1])
		}
	})
}
