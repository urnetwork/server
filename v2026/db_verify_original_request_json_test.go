// Fresh and historical SQL paths preserve raw originals while repairing only indexes.
package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Synthetic wire bytes deliberately include zero and non-UTF8 values; only
// their canonical base64 fields participate in the SQL request projection.
type verifyRequestJsonFixture struct {
	trailId, clientId         Id
	body, signature           []byte
	message, requestSignature []byte
}

// SQL migration fixtures need only the indexed fields. Model regressions use
// the complete real producer and independently validate both wire signatures.
func newVerifyRequestJsonFixture(t testing.TB) verifyRequestJsonFixture {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{23}, ed25519.SeedSize))
	fixture := verifyRequestJsonFixture{trailId: NewId(), clientId: NewId(), message: []byte{0, 255, 17, 0, 23}}
	fixture.requestSignature = ed25519.Sign(key, fixture.message)
	body := struct {
		Schema string `json:"schema"`
		Trail  struct {
			ClientId Id
		} `json:"trail"`
		RequestMessage   []byte `json:"request_message"`
		RequestSignature []byte `json:"request_signature"`
	}{Schema: "urnetwork-verify-original-transition-v1\x00", RequestMessage: fixture.message, RequestSignature: fixture.requestSignature}
	body.Trail.ClientId = fixture.clientId
	var err error
	fixture.body, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	fixture.signature = ed25519.Sign(key, fixture.body)
	return fixture
}

// Insert through the same triggers a rolling writer invokes, without a Go-side index.
func insertVerifyRequestJsonFixture(ctx context.Context, tx PgTx, fixture verifyRequestJsonFixture) {
	RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_original_transition
		(trail_id,previous_depth,observed_time,original_body,original_signature) VALUES($1,0,now(),$2,$3)`,
		fixture.trailId, fixture.body, fixture.signature))
}

// Projection identity and retained signed bytes must agree after every install path.
func assertVerifyRequestJsonFixture(t testing.TB, ctx context.Context, fixture verifyRequestJsonFixture) {
	t.Helper()
	Db(ctx, func(conn PgConn) {
		var body, signature, message, requestSignature []byte
		var clientId Id
		var scope string
		Raise(conn.QueryRow(ctx, `SELECT t.original_body,t.original_signature,r.client_id,
			r.scope_json::text,r.request_message,r.request_signature
			FROM verify_original_transition t JOIN verify_original_request_lookup r USING(trail_id,previous_depth)
			WHERE t.trail_id=$1 AND t.previous_depth=0`, fixture.trailId).
			Scan(&body, &signature, &clientId, &scope, &message, &requestSignature))
		if !bytes.Equal(body, fixture.body) || !bytes.Equal(signature, fixture.signature) || clientId != fixture.clientId || scope != "null" ||
			!bytes.Equal(message, fixture.message) || !bytes.Equal(requestSignature, fixture.requestSignature) {
			t.Fatal("request projection rewrote original custody or indexed identity")
		}
	})
}

// Originals accepted before request indexing must backfill despite their NUL
// domain, then remain byte-identical through closure and repair installation.
func TestVerifyOriginalRequestJsonBackfillsCanonicalDomain(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 772)
		fixture := newVerifyRequestJsonFixture(t)
		Tx(ctx, func(tx PgTx) { insertVerifyRequestJsonFixture(ctx, tx, fixture) })
		ApplyDbMigrationsUpTo(ctx, 773)
		assertVerifyRequestJsonFixture(t, ctx, fixture)
		ApplyDbMigrations(ctx)
		assertVerifyRequestJsonFixture(t, ctx, fixture)
	})
}

// Recreate an actual historical migration, including its original SQL identity.
// This fixture writes history only while constructing the disposable old database.
func applyLegacyVerifyRequestMigration(t testing.TB, ctx context.Context, index int, sql string) {
	t.Helper()
	identity, err := migrationIdentity(newSqlMigration(sql))
	if err != nil {
		t.Fatal(err)
	}
	if matches, err := MigrationIdentityMatches(index, identity); err != nil || !matches {
		t.Fatalf("historical fixture identity is not recognized: %s %v", identity, err)
	}
	MaintenanceTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, sql))
		Raise(recordMigrationIdentity(ctx, tx, index, identity))
		RaisePgResult(tx.Exec(ctx, `INSERT INTO migration_audit(start_version_number,end_version_number,status)
			VALUES($1,$2,'success')`, index, index+1))
	})
}

// Existing exact old catalog rows remain unchanged. The appended migration
// repairs both the rolling-writer capture and the permanent closure fence.
func TestVerifyOriginalRequestJsonRepairsInstalledReaders(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 772)
		applyLegacyVerifyRequestMigration(t, ctx, 772, testLegacyVerifyOriginalRequestSql)
		ApplyDbMigrationsUpTo(ctx, 774)
		applyLegacyVerifyRequestMigration(t, ctx, 774, testLegacyVerifyRequestClosureSql)
		ApplyDbMigrationsUpTo(ctx, 778)
		fixture := newVerifyRequestJsonFixture(t)
		recovered := HandleError(func() { Tx(ctx, func(tx PgTx) { insertVerifyRequestJsonFixture(ctx, tx, fixture) }) })
		legacyErr, ok := recovered.(error)
		var pgError *pgconn.PgError
		if !ok || !errors.As(legacyErr, &pgError) || pgError.Code != "22P05" {
			t.Fatal("historical reader fixture did not reproduce canonical domain rejection", recovered)
		}
		ApplyDbMigrations(ctx)
		Tx(ctx, func(tx PgTx) { insertVerifyRequestJsonFixture(ctx, tx, fixture) })
		assertVerifyRequestJsonFixture(t, ctx, fixture)
		blocked := newVerifyRequestJsonFixture(t)
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO verify_original_request_closed
				(request_hash,client_id,scope_json,request_message,request_signature,closure_original,receipt_body,receipt_signature)
				VALUES($1,$2,'null',$3,$4,$5,$5,$4)`, bytes.Repeat([]byte{29}, 32), blocked.clientId, blocked.message, blocked.requestSignature, []byte("synthetic-closed-original")))
		})
		recovered = HandleError(func() { Tx(ctx, func(tx PgTx) { insertVerifyRequestJsonFixture(ctx, tx, blocked) }) })
		fenceErr, ok := recovered.(error)
		if !ok || !errors.As(fenceErr, &pgError) || pgError.Code != "23514" || !strings.Contains(fenceErr.Error(), "verification request permanently closed") {
			t.Fatal("repaired rolling-writer fence admitted a closed exact request", recovered)
		}
		ApplyDbMigrations(ctx)
		for _, entry := range []migrationCatalogEntry{
			{Index: 772, Identity: "6166efbed5e36f5b416b2771cc96ebbc0a3aef9767a37e7e700201face01b212"},
			{Index: 774, Identity: "2780bc1901e2fafb9d1d53955ea2810597d68faddbab5db6d41141e8afe2a509"},
		} {
			MaintenanceDb(ctx, func(conn PgConn) {
				var recorded string
				Raise(conn.QueryRow(ctx, `SELECT trim(identity_sha256) FROM migration_catalog WHERE migration_index=$1`, entry.Index).Scan(&recorded))
				if recorded != entry.Identity {
					t.Fatal("repair rewrote durable migration history")
				}
			})
		}
	})
}

// Only the exact first schema member may be omitted from an index copy. Every
// other invalid escape, duplicate schema, and missing request field is refused.
func TestVerifyOriginalRequestJsonRejectsNoncanonicalProjection(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		fixture := newVerifyRequestJsonFixture(t)
		prefix := []byte(`{"schema":"urnetwork-verify-original-transition-v1\u0000",`)
		for _, test := range []struct {
			name string
			body []byte
		}{
			{name: "wrong schema", body: bytes.Replace(fixture.body, []byte("transition-v1"), []byte("transition-v2"), 1)},
			{name: "trailing object", body: append(bytes.Clone(fixture.body), []byte(`{}`)...)},
			{name: "duplicate schema", body: bytes.Replace(fixture.body, prefix, append(bytes.Clone(prefix), []byte(`"schema":"duplicate",`)...), 1)},
			{name: "NUL outside schema", body: bytes.Replace(fixture.body, prefix, append(bytes.Clone(prefix), []byte(`"scope":{"profile":"synthetic\u0000"},`)...), 1)},
			{name: "missing request", body: bytes.Replace(fixture.body, []byte(`"request_message"`), []byte(`"missing_message"`), 1)},
			{name: "missing client", body: bytes.Replace(fixture.body, []byte(`"ClientId"`), []byte(`"MissingId"`), 1)},
		} {
			changed := fixture
			changed.trailId = NewId()
			changed.body = test.body
			if recovered := HandleError(func() { Tx(t.Context(), func(tx PgTx) { insertVerifyRequestJsonFixture(t.Context(), tx, changed) }) }); recovered == nil {
				t.Fatalf("%s: malformed projection acquired durable custody", test.name)
			}
		}
		Tx(t.Context(), func(tx PgTx) { insertVerifyRequestJsonFixture(t.Context(), tx, fixture) })
		assertVerifyRequestJsonFixture(t, t.Context(), fixture)
	})
}

// Historical aliases are paired with the exact corrected SQL at the exact slot.
func TestVerifyOriginalRequestJsonCatalogAliasesStayExact(t *testing.T) {
	entries := testMigrationCatalogEntries(t, len(migrations))
	for _, index := range []int{772, 774} {
		oldSql := testLegacyVerifyOriginalRequestSql
		if index == 774 {
			oldSql = testLegacyVerifyRequestClosureSql
		}
		old, err := migrationIdentity(newSqlMigration(oldSql))
		if err != nil {
			t.Fatal(err)
		}
		current, err := MigrationIdentity(index)
		if err != nil || !matchesVerifyOriginalMigrationIdentity(index, current, old) {
			t.Fatal("exact historical SQL alias lost", index, err)
		}
		if matchesVerifyOriginalMigrationIdentity(index+1, current, old) || matchesVerifyOriginalMigrationIdentity(index, strings.Repeat("0", 64), old) ||
			matchesVerifyOriginalMigrationIdentity(index, current, strings.Repeat("0", 64)) {
			t.Fatal("historical alias admitted a different slot, current SQL, or recorded history")
		}
		entries[index].Identity = old
	}
	if err := validateMigrationCatalog(len(entries), entries); err != nil {
		t.Fatal(err)
	}
	entries[772].Identity = entries[774].Identity
	if err := validateMigrationCatalog(len(entries), entries); err == nil {
		t.Fatal("catalog accepted a cross-slot historical identity")
	}
}
