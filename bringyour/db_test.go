package bringyour


import (
	"context"
    "testing"
)


func TestMain(m *testing.M) {(&TestEnv{ApplyDbMigrations:false}).TestMain(m)}


// FIXME
// Id, *Id
// Db
// Tx
// Batch
// temp table
// temp join table


func Test1(t *testing.T) {

	ctx := context.Background()


    Db(ctx, func(conn PgConn) {
		_, err := conn.Exec(
			ctx,
			`
				CREATE TABLE test1(a uuid NOT NULL, b uuid NULL, c uuid NULL, PRIMARY KEY (a))
			`,
		)
		Raise(err)
	}, OptReadWrite())


	id1 := NewId()
	id2_ := NewId()
	var id2 Id
	var id3 *Id
	var id4 *Id

	Tx(ctx, func(tx PgTx) {
		_, err := tx.Exec(
			ctx,
			`
				INSERT INTO test1(a, b) VALUES ($1, $2)
			`,
			id1,
			id2_,
		)
		Raise(err)
	})

	Tx(ctx, func(tx PgTx) {
		result, err := tx.Query(
			ctx,
			`
				SELECT b FROM test1 WHERE a = $1
			`,
			id1,
		)
		WithPgResult(result, err, func() {
			if result.Next() {
				Raise(result.Scan(&id2))
			}
		})
	})

	Tx(ctx, func(tx PgTx) {
		_, err := tx.Exec(
			ctx,
			`
				INSERT INTO test1(a, b) VALUES ($1, $1)
			`,
			id2,
		)
		Raise(err)
	})

	Tx(ctx, func(tx PgTx) {
		result, err := tx.Query(
			ctx,
			`
				SELECT b, c FROM test1 WHERE a = $1
			`,
			id2,
		)
		WithPgResult(result, err, func() {
			if result.Next() {
				Raise(result.Scan(&id3, &id4))
			}
		})
	})

	if id2_ != id2 {
		t.Fatalf("X1")
	}

	if id3 == nil || id2 != *id3 {
		t.Fatalf("X2")
	}

	if id4 != nil {
		t.Fatalf("X3")
	}

	// id2_ == id2
	// id2 == *id3
	// id4 == nil

}

func Test2(t *testing.T) {
    
}

func Test3(t *testing.T) {
    
}


