package bringyour

import (
	"context"
	mathrand "math/rand"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

func TestIdPgCodec(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx := context.Background()

		Db(ctx, func(conn PgConn) {
			_, err := conn.Exec(
				ctx,
				`
				CREATE TABLE test(a uuid NOT NULL, b uuid NULL, c uuid NULL, PRIMARY KEY (a))
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
				INSERT INTO test(a, b) VALUES ($1, $2)
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
				SELECT b FROM test WHERE a = $1
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
				INSERT INTO test(a, b) VALUES ($1, $1)
			`,
				id2,
			)
			Raise(err)
		})

		Tx(ctx, func(tx PgTx) {
			result, err := tx.Query(
				ctx,
				`
				SELECT b, c FROM test WHERE a = $1
			`,
				id2,
			)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&id3, &id4))
				}
			})
		})

		assert.Equal(t, id2_, id2)
		assert.Equal(t, id2, *id3)
		assert.Equal(t, id4, nil)
	})
}

func TestBatch(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx := context.Background()

		Db(ctx, func(conn PgConn) {
			_, err := conn.Exec(
				ctx,
				`
				CREATE TABLE test(a uuid NOT NULL, b uuid NULL, c uuid NULL, PRIMARY KEY (a))
			`,
			)
			Raise(err)
		}, OptReadWrite())

		n := 10000

		Tx(ctx, func(tx PgTx) {
			BatchInTx(ctx, tx, func(batch PgBatch) {
				for i := 0; i < n; i += 1 {
					batch.Queue(
						`
						INSERT INTO test(a, b, c) VALUES ($1, $2, $3)
					`,
						NewId(),
						NewId(),
						NewId(),
					)
				}
			})
		})

		var k int

		Db(ctx, func(conn PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT COUNT(*) AS k FROM test
			`,
			)
			WithPgResult(result, err, func() {
				if result.Next() {
					Raise(result.Scan(&k))
				}
			})
		})

		assert.Equal(t, n, k)
	})
}

func TestTempTable(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx := context.Background()

		n := 1000
		ids := []Id{}
		for i := 0; i < n; i += 1 {
			ids = append(ids, NewId())
		}
		tempIds := map[Id]bool{}

		Tx(ctx, func(tx PgTx) {
			CreateTempTableInTx(ctx, tx, "temp1(a uuid)", ids...)
			result, err := tx.Query(
				ctx,
				`
				SELECT a FROM temp1
			`,
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					var id Id
					Raise(result.Scan(&id))
					tempIds[id] = true
				}
			})
		})

		assert.Equal(t, n, len(tempIds))
		for _, id := range ids {
			_, ok := tempIds[id]
			assert.Equal(t, ok, true)
		}

		idJoins := map[Id]Id{}
		for i := 0; i < n; i += 1 {
			idJoins[NewId()] = NewId()
		}
		tempIdJoins := map[Id]Id{}

		Tx(ctx, func(tx PgTx) {
			CreateTempJoinTableInTx(ctx, tx, "temp1(a uuid -> b uuid)", idJoins)
			result, err := tx.Query(
				ctx,
				`
				SELECT a, b FROM temp1
			`,
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					var a Id
					var b Id
					Raise(result.Scan(&a, &b))
					tempIdJoins[a] = b
				}
			})
		})

		assert.Equal(t, n, len(tempIdJoins))
		for a, b := range idJoins {
			b_, ok := tempIdJoins[a]
			assert.Equal(t, ok, true)
			assert.Equal(t, b_, b)
		}

		idCJoins := map[Id]C{}
		for i := 0; i < n; i += 1 {
			idCJoins[NewId()] = C{
				a: NewId(),
				b: NewId(),
				c: NewId(),
			}
		}
		tempIdCJoins := map[Id]C{}

		Tx(ctx, func(tx PgTx) {
			CreateTempJoinTableInTx(ctx, tx, "temp1(a uuid -> ca uuid, cb uuid, cc uuid)", idCJoins)
			result, err := tx.Query(
				ctx,
				`
				SELECT a, ca, cb, cc FROM temp1
			`,
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					var a Id
					var c C
					Raise(result.Scan(&a, &c.a, &c.b, &c.c))
					tempIdCJoins[a] = c
				}
			})
		})

		assert.Equal(t, n, len(tempIdJoins))
		for a, c := range idCJoins {
			c_, ok := tempIdCJoins[a]
			assert.Equal(t, ok, true)
			assert.Equal(t, c_, c)
		}
	})
}

type C struct {
	a Id
	b Id
	c Id
}

func (self *C) Values() []any {
	return []any{self.a, self.b, self.c}
}

func TestRetry(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx := context.Background()

		Db(ctx, func(conn PgConn) {
			_, err := conn.Exec(
				ctx,
				`
				CREATE TABLE test(a uuid NOT NULL, PRIMARY KEY (a))
			`,
			)
			Raise(err)
		}, OptReadWrite())

		n := 10
		ids := []Id{}
		for i := 0; i < n; i += 1 {
			ids = append(ids, NewId())
		}

		// now insert ids to trigger conflict
		for i := 0; i < n; i += 1 {
			j := -1
			Tx(ctx, func(tx PgTx) {
				// increments on each retry until an ids[j] has not been inserted
				j += 1
				tx.Exec(
					ctx,
					`
					INSERT INTO test (a) VALUES ($1)
				`,
					ids[j],
				)
			})
		}

		testIds := map[Id]bool{}
		Tx(ctx, func(tx PgTx) {
			result, err := tx.Query(
				ctx,
				`
				SELECT a FROM test
			`,
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					var id Id
					Raise(result.Scan(&id))
					testIds[id] = true
				}
			})
		})

		assert.Equal(t, n, len(testIds))
		for _, id := range ids {
			_, ok := testIds[id]
			assert.Equal(t, ok, true)
		}
	})
}

func TestRetryInnerError(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx := context.Background()

		Db(ctx, func(conn PgConn) {
			_, err := conn.Exec(
				ctx,
				`
				CREATE TABLE test(a uuid NOT NULL, PRIMARY KEY (a))
			`,
			)
			Raise(err)
		}, OptReadWrite())

		n := 10
		ids := []Id{}
		for i := 0; i < n; i += 1 {
			ids = append(ids, NewId())
		}

		// now insert ids to trigger conflict
		for i := 0; i < n; i += 1 {
			j := -1
			Tx(ctx, func(tx PgTx) {
				// increments on each retry until an ids[j] has not been inserted
				// cause an error inside the transaction via `RaisePgResult`
				j += 1
				RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO test (a) VALUES ($1)
				`,
					ids[j],
				))
			})
		}

		testIds := map[Id]bool{}
		Tx(ctx, func(tx PgTx) {
			result, err := tx.Query(
				ctx,
				`
				SELECT a FROM test
			`,
			)
			WithPgResult(result, err, func() {
				for result.Next() {
					var id Id
					Raise(result.Scan(&id))
					testIds[id] = true
				}
			})
		})

		assert.Equal(t, n, len(testIds))
		for _, id := range ids {
			_, ok := testIds[id]
			assert.Equal(t, ok, true)
		}
	})
}

func TestSerializableTx(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx := context.Background()

		k := 5
		n := 10
		ids := []Id{}
		for i := 0; i < n; i += 1 {
			ids = append(ids, NewId())
		}

		Db(ctx, func(conn PgConn) {
			_, err := conn.Exec(
				ctx,
				`
				CREATE TABLE test(a uuid NOT NULL, b int NOT NULL, PRIMARY KEY (a))
			`,
			)
			Raise(err)

			for _, id := range ids {
				_, err := conn.Exec(
					ctx,
					`
					INSERT INTO test(a, b) VALUES ($1, $2)
				`,
					id,
					0,
				)
				Raise(err)
			}
		}, OptReadWrite())

		end := make(chan error, k)

		// now insert ids to trigger conflict
		// conflicting transactions should retry until all are updated
		for i := 0; i < k; i += 1 {
			for _, id := range ids {
				go func() {
					Tx(ctx, func(tx PgTx) {

						result, err := tx.Query(
							ctx,
							`
							SELECT b FROM test
							WHERE a = $1
						`,
							id,
						)
						var b int
						WithPgResult(result, err, func() {
							ok := result.Next()
							assert.Equal(t, true, ok)
							Raise(result.Scan(&b))
						})

						timeout := time.Duration(mathrand.Intn(1000)) * time.Millisecond
						select {
						case <-ctx.Done():
							return
						case <-time.After(timeout):
						}

						tag := RaisePgResult(tx.Exec(
							ctx,
							`
							UPDATE test SET
							b = $2 + 1
							WHERE a = $1
						`,
							id,
							b,
						))
						assert.Equal(t, 1, int(tag.RowsAffected()))
					}, TxSerializable)

					end <- nil
				}()
			}
		}

		for i := 0; i < k*len(ids); i += 1 {
			select {
			case err := <-end:
				assert.Equal(t, err, nil)
			case <-time.After(60 * time.Second):
				t.FailNow()
			}
		}

		Db(ctx, func(conn PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT a, b FROM test
			`,
			)
			out := map[Id]int{}
			WithPgResult(result, err, func() {
				for result.Next() {
					var a Id
					var b int
					Raise(result.Scan(&a, &b))
					out[a] = b
				}
			})
			assert.Equal(t, n, len(out))
			for _, b := range out {
				assert.Equal(t, b, k)
			}
		})
	})
}

func TestUpdateTx(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx := context.Background()

		k := 5
		n := 10
		ids := []Id{}
		for i := 0; i < n; i += 1 {
			ids = append(ids, NewId())
		}

		Db(ctx, func(conn PgConn) {
			_, err := conn.Exec(
				ctx,
				`
				CREATE TABLE test(a uuid NOT NULL, b int NOT NULL, PRIMARY KEY (a))
			`,
			)
			Raise(err)

			for _, id := range ids {
				_, err := conn.Exec(
					ctx,
					`
					INSERT INTO test(a, b) VALUES ($1, $2)
				`,
					id,
					0,
				)
				Raise(err)
			}
		}, OptReadWrite())

		end := make(chan error, k)

		// now insert ids to trigger conflict
		// conflicting transactions will order due to `FOR UPDATE`
		for i := 0; i < k; i += 1 {
			for _, id := range ids {
				go func() {
					Tx(ctx, func(tx PgTx) {
						result, err := tx.Query(
							ctx,
							`
							SELECT b FROM test
							WHERE a = $1
							FOR UPDATE
						`,
							id,
						)
						var b int
						WithPgResult(result, err, func() {
							ok := result.Next()
							assert.Equal(t, true, ok)
							Raise(result.Scan(&b))
						})

						timeout := time.Duration(mathrand.Intn(1000)) * time.Millisecond
						select {
						case <-ctx.Done():
							return
						case <-time.After(timeout):
						}

						tag := RaisePgResult(tx.Exec(
							ctx,
							`
							UPDATE test SET
							b = $2 + 1
							WHERE a = $1 AND b = $2
						`,
							id,
							b,
						))
						assert.Equal(t, 1, int(tag.RowsAffected()))
					}, TxReadCommitted)

					end <- nil
				}()
			}
		}

		for i := 0; i < k*len(ids); i += 1 {
			select {
			case err := <-end:
				assert.Equal(t, err, nil)
			case <-time.After(60 * time.Second):
				t.FailNow()
			}
		}

		Db(ctx, func(conn PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT a, b FROM test
			`,
			)
			out := map[Id]int{}
			WithPgResult(result, err, func() {
				for result.Next() {
					var a Id
					var b int
					Raise(result.Scan(&a, &b))
					out[a] = b
				}
			})
			assert.Equal(t, n, len(out))
			for _, b := range out {
				assert.Equal(t, b, k)
			}
		})
	})
}

// TODO test that a non-constraint error like a syntax error gets panic'd correctly
