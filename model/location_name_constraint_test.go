package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The Go guards in CreateLocation are the first line, not the last one. They
// only bind callers that go through CreateLocation, and the 163 blank rows on
// beta are the standing proof that a caller can write this table with a name
// nobody checked. These tests are about the database refusing it underneath
// every caller, present and future.
//
// The migration that backfills those rows and adds the constraint is the last
// entry in server.migrations; the test environment applies it like any other,
// so a fresh test database arrives here already constrained.

// TestLocationNameNotBlankConstraintExists names the constraint in the failure
// message. Without it the two tests below fail as "the insert was accepted",
// which is true but says nothing about why.
func TestLocationNameNotBlankConstraintExists(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		var definition string
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
                    SELECT pg_get_constraintdef(oid)
                    FROM pg_constraint
                    WHERE
                        conrelid = 'location'::regclass AND
                        conname = 'location_name_not_blank'
                `,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&definition))
				}
			})
		})

		if definition == "" {
			t.Fatal("the location_name_not_blank CHECK constraint is not on the location table")
		}
	})
}

// TestLocationBlankNameRejectedByDb goes around CreateLocation deliberately --
// a raw INSERT is what a future caller, a fixture, or a hand-run repair script
// looks like, and none of them consult the Go resolution.
func TestLocationBlankNameRejectedByDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		insert := func(locationName string) error {
			locationId := server.NewId()
			var insertErr error
			server.Db(ctx, func(conn server.PgConn) {
				_, insertErr = conn.Exec(
					ctx,
					`
                        INSERT INTO location (
                            location_id,
                            location_type,
                            location_name,
                            country_location_id,
                            country_code,
                            location_full_name
                        )
                        VALUES ($1, $2, $3, $1, $4, $5)
                    `,
					locationId,
					LocationTypeCountry,
					locationName,
					"xx",
					fmt.Sprintf("constraint-test-%s", locationId),
				)
			})
			return insertErr
		}

		// the row the location-group member path wrote 161 times
		if err := insert(""); err == nil {
			t.Fatal("a blank location_name was accepted by the database")
		}

		// the constraint must reject blank names, not names
		if err := insert("Constraintia"); err != nil {
			t.Fatalf("a named location was rejected: %s", err)
		}
	})
}

// TestLocationBlankNameRejectedOnUpdate covers the other half: a row can be
// emptied after it is created, and an UPDATE never passes through
// CreateLocation's resolution at all.
func TestLocationBlankNameRejectedOnUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		location := &Location{
			LocationType: LocationTypeCountry,
			CountryCode:  "cn",
		}
		CreateLocation(ctx, location)
		connect.AssertEqual(t, locationName(ctx, t, location.LocationId), "China")

		var updateErr error
		server.Db(ctx, func(conn server.PgConn) {
			_, updateErr = conn.Exec(
				ctx,
				`UPDATE location SET location_name = '' WHERE location_id = $1`,
				location.LocationId,
			)
		})

		if updateErr == nil {
			t.Fatal("a location_name was blanked by an UPDATE")
		}
		connect.AssertEqual(t, locationName(ctx, t, location.LocationId), "China")
		connect.AssertEqual(t, blankNamedLocationCount(ctx, t), 0)
	})
}
