package model

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server/v2026"
)

// The contract boundary locks client rows FOR KEY SHARE. A key-share lock
// conflicts with FOR UPDATE only: DELETE and key updates take that lock
// themselves, but a plain UPDATE of a lifecycle column takes FOR NO KEY UPDATE
// and would let a contract proceed on a client that is being deactivated. So
// every production statement that sets a network_client lifecycle column must
// run after an explicit FOR UPDATE of its rows. The reviewed writers prove
// their order in TestContractLifecycleWritesUsePrimaryDatabaseClockAfterLocks;
// any new writer fails here until it is reviewed the same way.
func TestNetworkClientLifecycleWritesFollowExplicitRowLocks(t *testing.T) {
	lifecycleWrite := regexp.MustCompile(`(?is)\bupdate\s+network_client\b(?:\s+(?:as\s+)?[a-z_][a-z0-9_]*)?\s+set\s+(.*?)(?:\bfrom\b|\bwhere\b|\breturning\b|$)`)
	lifecycleColumn := regexp.MustCompile(`(?is)(?:^|[\s,])(active|deactivate_time|network_id|client_id|source_client_id)\s*=|\(([^)]*)\)\s*=`)
	lifecycleColumnName := regexp.MustCompile(`(?i)\b(active|deactivate_time|network_id|client_id|source_client_id)\b`)
	setsLifecycle := func(statement string) bool {
		for _, write := range lifecycleWrite.FindAllStringSubmatch(statement, -1) {
			for _, assignment := range lifecycleColumn.FindAllStringSubmatch(write[1], -1) {
				// a single column assignment, or a column list assigned at once
				if assignment[1] != "" || lifecycleColumnName.MatchString(assignment[2]) {
					return true
				}
			}
		}
		return false
	}

	// Every production source of the module that names the table.
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	writers := []string{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(bytes.ToLower(data), []byte("network_client")) {
			return nil
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, data, 0)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			name := ""
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				name = declaration.Name.Name
			case *ast.GenDecl:
				name = declaration.Tok.String()
			}
			ast.Inspect(declaration, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("%s: parse string literal: %v", relative, err)
				}
				if setsLifecycle(value) {
					writers = append(writers, filepath.ToSlash(relative)+": "+name)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(writers)
	reviewed := []string{
		"model/network_client_model.go: RemoveDisconnectedNetworkClients",
		"model/network_client_model.go: deactivateLockedNetworkClientsInTx",
	}
	if !slices.Equal(writers, reviewed) {
		t.Fatalf("network_client lifecycle writers = %v, want exactly the reviewed writers %v", writers, reviewed)
	}

	// The guard recognizes every spelling a writer could use.
	for _, statement := range []string{
		`UPDATE network_client SET active=false WHERE client_id = $1`,
		`update network_client as c set deactivate_time = $2, active = false where c.client_id = $1`,
		`UPDATE network_client SET (active, deactivate_time) = (false, $2) WHERE client_id = $1`,
		`WITH clock AS (SELECT now() AS t) UPDATE network_client SET network_id = $2 FROM clock WHERE client_id = $1`,
	} {
		if !setsLifecycle(statement) {
			t.Errorf("guard misses lifecycle write %q", statement)
		}
	}
	for _, statement := range []string{
		`UPDATE network_client SET auth_time = $2 WHERE client_id = $1 AND active = true`,
		`UPDATE network_client AS top SET contract_time = $2 FROM network_client AS payer WHERE payer.client_id = $1`,
		`UPDATE network_client SET session_id=$3,session_create_time=$4 WHERE client_id=$1 AND network_id=$2`,
		`UPDATE network_client_connection SET connected = false WHERE client_id = $1`,
	} {
		if setsLifecycle(statement) {
			t.Errorf("guard flags non-lifecycle write %q", statement)
		}
	}
}

// Auth and contract time stamps update network_client without touching its
// key or lifecycle columns. Under FOR SHARE an open stamp held every contract
// to that client until the stamp committed or the 2 s lock bound refused the
// contract; a key-share boundary shares the row with it.
func TestContractBoundaryDoesNotWaitForNonKeyClientWrites(t *testing.T) {
	for _, write := range []struct {
		name string
		sql  string
	}{
		{name: "auth time", sql: `UPDATE network_client SET auth_time = $2 WHERE client_id = $1`},
		{name: "contract time", sql: `UPDATE network_client SET contract_time = $2 WHERE client_id = $1`},
	} {
		t.Run(write.name, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
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

				writerConn := acquireContractLifecycleTestConnection(t, ctx)
				defer writerConn.Release()
				writerTx, err := writerConn.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					_ = writerTx.Rollback(context.Background())
				}()
				for _, clientId := range []server.Id{sourceId, destinationId} {
					if _, err := writerTx.Exec(ctx, write.sql, clientId, server.NowUtc()); err != nil {
						t.Fatal(err)
					}
				}

				// both stamps stay open while the contract is created
				var contractId server.Id
				var createErr error
				server.HandleError(func() {
					contractId, createErr = CreateContractNoEscrow(
						ctx,
						sourceNetworkId,
						sourceId,
						destinationNetworkId,
						destinationId,
						1024,
					)
				}, func(err error) {
					createErr = err
				})
				if createErr != nil {
					t.Fatalf("contract creation behind open %s stamps: %v", write.name, createErr)
				}
				if contractId == (server.Id{}) {
					t.Fatal("contract creation behind open stamps returned no contract")
				}
				if err := writerTx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if count := contractLifecycleTestCount(t, ctx, sourceId, destinationId); count != 1 {
					t.Fatalf("contract count = %d, want 1", count)
				}
			})
		})
	}
}

// Begins a repeatable-read transaction on its own session and takes its
// snapshot with a first statement.
func beginContractBoundaryRepeatableRead(t testing.TB, ctx context.Context) (server.PgConn, pgx.Tx) {
	t.Helper()
	conn := acquireContractLifecycleTestConnection(t, ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		conn.Release()
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		_ = tx.Rollback(context.Background())
		conn.Release()
		t.Fatal(err)
	}
	return conn, tx
}

// The boundary's verdict, with a raised statement error as its error.
func contractBoundaryVerdict(ctx context.Context, tx server.PgTx, networkId server.Id, sourceId server.Id, destinationId server.Id) (verdict error) {
	server.HandleError(func() {
		verdict = lockActiveContractClientsInTx(ctx, tx, networkId, sourceId, networkId, destinationId)
	}, func(err error) {
		verdict = err
	})
	return
}

// A repeatable-read boundary whose snapshot predates a committed stamp failed
// with a serialization error (40001) under FOR SHARE. A non-key update leaves
// a key-share lock nothing to conflict with, so the boundary proceeds.
func TestContractBoundaryRepeatableReadToleratesCommittedNonKeyWrite(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		networkId := server.NewId()
		sourceId := server.NewId()
		destinationId := server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
			sourceId:      networkId,
			destinationId: networkId,
		})

		readerConn, readerTx := beginContractBoundaryRepeatableRead(t, ctx)
		defer readerConn.Release()
		defer func() {
			_ = readerTx.Rollback(context.Background())
		}()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET auth_time = $2 WHERE client_id = ANY($1)`,
				[]server.Id{sourceId, destinationId},
				server.NowUtc(),
			))
		}, server.TxReadCommitted)

		if verdict := contractBoundaryVerdict(ctx, readerTx, networkId, sourceId, destinationId); verdict != nil {
			t.Fatalf("repeatable-read boundary over a committed stamp: %v", verdict)
		}
	})
}

// Every lifecycle writer locks FOR UPDATE before its non-key UPDATE, and
// PostgreSQL records that row version as a key update. So a repeatable-read
// boundary whose snapshot predates a committed deactivation fails with a
// serialization error (40001) under FOR KEY SHARE as under FOR SHARE: it never
// proceeds on the stale active row it still sees.
func TestContractBoundaryRepeatableReadRefusesDeactivationAfterSnapshot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		networkId := server.NewId()
		sourceId := server.NewId()
		destinationId := server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{
			sourceId:      networkId,
			destinationId: networkId,
		})

		readerConn, readerTx := beginContractBoundaryRepeatableRead(t, ctx)
		defer readerConn.Release()
		defer func() {
			_ = readerTx.Rollback(context.Background())
		}()
		server.Tx(ctx, func(tx server.PgTx) {
			rowCount, err := deactivateNetworkClientsInTx(ctx, tx, []server.Id{destinationId}, networkId)
			server.Raise(err)
			if rowCount != 1 {
				panic(errors.New("deactivation wrote no row"))
			}
		}, server.TxReadCommitted)

		verdict := contractBoundaryVerdict(ctx, readerTx, networkId, sourceId, destinationId)
		var pgErr *pgconn.PgError
		if !errors.As(verdict, &pgErr) || pgErr.Code != pgerrcode.SerializationFailure {
			t.Fatalf("repeatable-read boundary over a committed deactivation = %v, want a serialization failure", verdict)
		}
	})
}
