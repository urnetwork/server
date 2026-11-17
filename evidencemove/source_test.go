package evidencemove

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The source reads st_client_key_history in primary key order in bounded
// batches, from a read-only process: every row once, in order, each with its
// evidence's byte length, and none after the end. The sample and the row
// estimate read the same table.
func TestClientKeyHistorySourceReadsEveryRowInKeyOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		domain := sha256.Sum256([]byte("evidencemove-source-test"))
		type row struct {
			cursor Cursor
			hash   string
			size   int64
		}
		want := []row{}
		for client := range 9 {
			clientId := server.NewId()
			for generation := int64(1); generation <= int64(1+client%3); generation += 1 {
				registration := sha256.Sum256(fmt.Appendf(nil, "%s/%d", clientId, generation))
				evidence := fmt.Appendf(nil, `{"client":%d,"generation":%d,"padding":"%s"}`, client, generation, strings.Repeat("x", client*7))
				hash := "sha256:" + hex.EncodeToString(registration[:])
				server.MaintenanceTx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(
						ctx,
						`INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
						VALUES ($1, $2, $3, $4, $5, $6)`,
						clientId, generation, domain[:], registration[:], hash, evidence,
					))
				})
				want = append(want, row{cursor: Cursor{ClientId: clientId, Generation: generation}, hash: hash, size: int64(len(evidence))})
			}
		}
		slices.SortFunc(want, func(a row, b row) int { return compareCursors(a.cursor, b.cursor) })
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE st_client_key_history`))
		})

		server.RequirePgReadOnly()
		// before the environment drops its database
		defer server.Testing_ReleasePgReadOnly()
		source := &ClientKeyHistorySource{}
		got := []row{}
		cursor := Cursor{}
		for {
			items, err := source.Next(ctx, cursor, 4)
			if err != nil {
				t.Fatal(err)
			}
			if 4 < len(items) {
				t.Fatalf("a batch of 4 returned %d", len(items))
			}
			if len(items) == 0 {
				break
			}
			for _, item := range items {
				got = append(got, row{cursor: item.Cursor, hash: item.Hash, size: item.Size})
			}
			cursor = items[len(items)-1].Cursor
		}
		if !slices.Equal(got, want) {
			t.Fatalf("read %d rows %v\nwant %d rows %v", len(got), got, len(want), want)
		}
		estimate, pages, err := ClientKeyHistoryRowEstimate(ctx)
		if err != nil || estimate != int64(len(want)) || pages < 1 {
			t.Fatalf("estimate %d rows on %d pages, %v; want %d rows", estimate, pages, err, len(want))
		}
		sample, err := SampleClientKeyHistory(ctx, 1000, pages, 1)
		if err != nil || len(sample) != len(want) {
			t.Fatalf("sampled %d rows, %v; want all %d", len(sample), err, len(want))
		}
		// the process really is read only
		writeRefused := false
		func() {
			defer func() {
				writeRefused = recover() != nil
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM st_client_key_head`))
			}, server.OptNoRetry())
		}()
		if !writeRefused {
			t.Fatal("the source's process accepted a write")
		}
	})
}
