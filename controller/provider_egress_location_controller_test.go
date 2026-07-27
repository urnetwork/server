package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestSubmitProviderEgressLocationCountryOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		res, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "US",
			Country:          "United States",
			ASN:              401486,
			Org:              "RAVNIX LLC",
			Hosting:          true,
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)
		if res.LocationId == (server.Id{}) {
			t.Fatal("expected a resolved location id")
		}

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		connect.AssertEqual(t, stored.CountryCode, "us")
		connect.AssertEqual(t, stored.ASN, 401486)
		connect.AssertEqual(t, stored.Hosting, true)
		connect.AssertEqual(t, stored.CityConfident, false)

		// the resolved location must be the country-granular row, with no
		// city/region association
		loc := model.GetLocation(ctx, stored.LocationId)
		if loc == nil {
			t.Fatal("expected the resolved location row to exist")
		}
		connect.AssertEqual(t, loc.LocationType, model.LocationTypeCountry)
		if loc.CityLocationId != (server.Id{}) {
			t.Fatal("a country-granularity row must not have a city association")
		}
		if loc.RegionLocationId != (server.Id{}) {
			t.Fatal("a country-granularity row must not have a region association")
		}
	})
}

func TestSubmitProviderEgressLocationRejectsNotCountryConfident(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			CountryConfident: false,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("a submission that is not country-confident must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

func TestSubmitProviderEgressLocationRejectsUnknownClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         server.NewId(), // never created
			CountryCode:      "us",
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("unknown client_id must be rejected")
		}
	})
}

func TestSubmitProviderEgressLocationCityConfidentStoresCity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		// the ingest path resolves against locations that ALREADY exist and
		// never creates one, so the city has to be in the table first -- as it
		// would be from the mmdb import
		denver := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Denver",
			Region:       "Colorado",
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, denver)

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			Region:           "Colorado",
			City:             "Denver",
			CountryConfident: true,
			CityConfident:    true,
			ObservedAt:       server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		connect.AssertEqual(t, stored.CityConfident, true)

		// the resolved location must be the city-granular row that already
		// existed, not a new one
		connect.AssertEqual(t, stored.LocationId, denver.LocationId)
		loc := model.GetLocation(ctx, stored.LocationId)
		if loc == nil {
			t.Fatal("expected the resolved location row to exist")
		}
		connect.AssertEqual(t, loc.LocationType, model.LocationTypeCity)
	})
}

// An empty Country must be rejected, not silently stored: model.CreateLocation
// dedupes country rows on (location_type, country_code), so an empty name
// would create a canonical, permanently-blank row that every later lookup for
// that country reuses forever.
func TestSubmitProviderEgressLocationRejectsEmptyCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "   ", // blank after trimming
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("an empty country must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// A city-confident submission with an empty City must be rejected rather than
// silently falling back to country granularity: the same empty-canonical-row
// corruption applies to city/region rows.
func TestSubmitProviderEgressLocationRejectsEmptyCityWhenCityConfident(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			Region:           "Colorado",
			City:             "",
			CountryConfident: true,
			CityConfident:    true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("a city-confident submission with an empty city must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// A city-confident submission with an empty Region must likewise be rejected:
// the region row is created with the same dedupe-on-empty-name hazard.
func TestSubmitProviderEgressLocationRejectsEmptyRegionWhenCityConfident(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			Region:           "",
			City:             "Denver",
			CountryConfident: true,
			CityConfident:    true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("a city-confident submission with an empty region must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// An over-long Country must be rejected with a clear error instead of
// panicking inside model.CreateLocation on a Postgres "value too long for
// type character varying(128)" error.
func TestSubmitProviderEgressLocationRejectsOverLongCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          strings.Repeat("a", 129),
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("an over-long country must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// An over-long Org must likewise be rejected rather than panicking inside
// storage.
func TestSubmitProviderEgressLocationRejectsOverLongOrg(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			Org:              strings.Repeat("a", 257),
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		if err == nil {
			t.Fatal("an over-long org must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

func TestSubmitProviderEgressLocationRejectsStaleObservedAt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			CountryConfident: true,
			ObservedAt:       server.NowUtc().Add(-30 * 24 * time.Hour),
		})
		if err == nil {
			t.Fatal("a submission observed long ago must be rejected")
		}
	})
}

// A far-future observed_at must be rejected, not just an old one: unchecked,
// it would defeat the monotonic upsert (it always "wins"), read as fresh
// forever, and outlive the taskworker sweep -- permanently pinning a
// provider's location with no API-side recovery. See
// MaxProviderEgressLocationSubmissionSkew.
func TestSubmitProviderEgressLocationRejectsFutureObservedAt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "jp",
			Country:          "Japan",
			CountryConfident: true,
			ObservedAt:       server.NowUtc().Add(10 * 365 * 24 * time.Hour),
		})
		if err == nil {
			t.Fatal("a far-future observed_at must be rejected")
		}
		if model.GetProviderEgressLocation(ctx, clientId) != nil {
			t.Fatal("rejected submission must not be stored")
		}
	})
}

// A submission within the allowed clock-skew window must still be accepted:
// the future-timestamp rejection must not be so strict that ordinary clock
// drift between the prober and server breaks legitimate submissions.
func TestSubmitProviderEgressLocationAcceptsWithinSkewObservedAt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		res, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			CountryConfident: true,
			ObservedAt:       server.NowUtc().Add(1 * time.Minute),
		})
		connect.AssertEqual(t, err, nil)
		if res.LocationId == (server.Id{}) {
			t.Fatal("expected a resolved location id")
		}

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
	})
}

// A large 32-bit ASN (e.g. from the private-use range, common on
// hosting/VPN infrastructure) must round-trip cleanly, not panic. The asn
// column used to be `int` (Postgres int4, max ~2.147e9); ASNs are 32-bit
// unsigned (max ~4.295e9), so a value above int4's range panicked deep in
// pgx's arg encoding.
func TestSubmitProviderEgressLocationAcceptsLargeAsn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		const largeAsn = 4200000000

		res, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "us",
			Country:          "United States",
			ASN:              largeAsn,
			CountryConfident: true,
			ObservedAt:       server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)
		if res.LocationId == (server.Id{}) {
			t.Fatal("expected a resolved location id")
		}

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		connect.AssertEqual(t, stored.ASN, largeAsn)
	})
}

// testing_countLocations is the whole point of the two tests below: the
// `location` table is shared with the provider list and the location search,
// its rows are permanent, and nothing cleans up a bad one. An ingest endpoint
// that can add to it is an endpoint that can corrupt it from outside.
func testing_countLocations(ctx context.Context) int64 {
	var count int64
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT COUNT(*) FROM location`)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// A city-confident submission whose city is not already in the location table
// must fall back to country granularity and must NOT create a location row.
//
// model.CreateLocation dedupes a city on its exact location_name, so before
// this fix an unrecognised spelling did not fail -- it silently inserted a new
// permanent row and indexed it for search. The prober's consensus stores the
// winning source's original display string and the three free geolocation
// sources demonstrably disagree on spelling, so "Frankfurt am Main",
// "Frankfurt Am Main" and "Frankfurt/Main" would each have become their own
// row. Those rows outlive a code revert and there is no cleanup path.
//
// Reverting the MatchExistingLocation call in SubmitProviderEgressLocation must
// fail this test: the row count goes up and the stored location is a city.
func TestSubmitProviderEgressLocationUnknownCityDoesNotCreateALocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		// Germany, and one real German city, already exist -- as they would
		// from the mmdb import. The submission below names a DIFFERENT city
		// that has never been seen.
		model.CreateLocation(ctx, &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Frankfurt am Main",
			Region:       "Hesse",
			Country:      "Germany",
			CountryCode:  "de",
		})

		before := testing_countLocations(ctx)

		res, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "de",
			Country:          "Germany",
			Region:           "Hesse",
			City:             "Kleinstadt Nirgendwo",
			CountryConfident: true,
			CityConfident:    true,
			ObservedAt:       server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)

		// nothing was added to the shared table
		after := testing_countLocations(ctx)
		if after != before {
			t.Errorf("location row count went from %d to %d; an unmatched city must not create a permanent row in the shared location table", before, after)
		}

		// and the submission was stored at country granularity instead
		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		loc := model.GetLocation(ctx, stored.LocationId)
		if loc == nil {
			t.Fatal("expected the resolved location row to exist")
		}
		if loc.LocationType != model.LocationTypeCountry {
			t.Errorf("stored location_type = %q, want %q: an unmatched city must fall back to country granularity", loc.LocationType, model.LocationTypeCountry)
		}
		connect.AssertEqual(t, res.LocationId, stored.LocationId)

		// city_confident tracks the granularity actually stored, so the row
		// stays internally consistent: location_id is a city row exactly when
		// city_confident is set
		if stored.CityConfident {
			t.Error("city_confident must be false when the submission was stored at country granularity")
		}
	})
}

// The variants that matter are the ones the geolocation sources actually
// produce for one place: different case, punctuation or spacing, and -- the
// case this feature was built for -- a parenthesised district qualifier
// ("Frankfurt am Main (Innenstadt I)"). Those must resolve to the row that is
// already there; discarding them to country throws away real precision, and
// creating a row for each is the bug this guards against.
//
// An earlier revision of this list only contained foldings the implementation
// already handled, so it could not fail. The qualifier case below fails against
// a matcher that only drops punctuation, which is what makes this test worth
// running. Diacritics -- the other class that failed -- have their own test.
func TestSubmitProviderEgressLocationMatchesCitySpellingVariant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), server.NewId(), "", "")

		frankfurt := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Frankfurt am Main",
			Region:       "Hesse",
			Country:      "Germany",
			CountryCode:  "de",
		}
		model.CreateLocation(ctx, frankfurt)

		before := testing_countLocations(ctx)

		// each of these is a real disagreement between the geolocation sources
		// over the same place
		variants := []struct{ region, city string }{
			{"Hesse", "Frankfurt am Main"}, // exact
			{"Hesse", "Frankfurt Am Main"}, // case
			{"hesse", "FRANKFURT AM MAIN"}, // case, both levels
			{"Hesse", "Frankfurt-am-Main"}, // punctuation
			{"Hesse", " Frankfurt am Main "},
			// the observed disagreement that motivated matching at all: one
			// source appends the district. Dropping "(" and ")" as punctuation
			// is not enough -- the qualifier's letters stay in the key -- so
			// this misses unless the qualifier itself is stripped.
			{"Hesse", "Frankfurt am Main (Innenstadt I)"},
			{"Hesse", "Frankfurt am Main (Innenstadt I) "},
			// qualifier on the region too
			{"Hesse (Regierungsbezirk Darmstadt)", "Frankfurt am Main"},
		}
		for _, variant := range variants {
			label := fmt.Sprintf("%q/%q", variant.region, variant.city)
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

			_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
				ClientId:         clientId,
				CountryCode:      "DE",
				Country:          "Germany",
				Region:           variant.region,
				City:             variant.city,
				CountryConfident: true,
				CityConfident:    true,
				ObservedAt:       server.NowUtc(),
			})
			connect.AssertEqual(t, err, nil)

			stored := model.GetProviderEgressLocation(ctx, clientId)
			if stored == nil {
				t.Fatalf("%s: expected the submission to be stored", label)
			}
			if stored.LocationId != frankfurt.LocationId {
				t.Errorf("%s resolved to %s, want the existing Frankfurt row %s", label, stored.LocationId, frankfurt.LocationId)
			}
			if !stored.CityConfident {
				t.Errorf("%s: city_confident must stay set when the city resolved", label)
			}
		}

		if after := testing_countLocations(ctx); after != before {
			t.Errorf("location row count went from %d to %d; spelling variants must reuse the existing row, not add new ones", before, after)
		}
	})
}

// Diacritics are the largest single class of spelling disagreement between the
// geolocation sources: one emits the local spelling ("São Paulo", "Zürich",
// "Kraków"), another an ASCII transliteration ("Sao Paulo", "Zurich",
// "Krakow"), and the mmdb import that seeded the existing rows picked one of
// the two per city with no way to know which. Every one of these missed before
// the NFD fold and fell back to country -- and, per
// TestSetConnectionLocationProbedCountryDoesNotCoarsenMmdbCity, a fallback on a
// provider the mmdb already placed in a city used to be an outright
// discoverability regression.
//
// Both directions are exercised, because which side carries the accent depends
// on how the existing row happened to be seeded.
func TestSubmitProviderEgressLocationMatchesAccentedCityVariant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()

		// existing rows, seeded the way the mmdb import would have: some
		// accented, some already transliterated
		saoPaulo := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "São Paulo",
			Region:       "São Paulo",
			Country:      "Brazil",
			CountryCode:  "br",
		}
		model.CreateLocation(ctx, saoPaulo)

		zurich := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Zürich",
			Region:       "Zürich",
			Country:      "Switzerland",
			CountryCode:  "ch",
		}
		model.CreateLocation(ctx, zurich)

		// seeded WITHOUT the accent, so the probe is the side carrying it
		krakow := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Krakow",
			Region:       "Lesser Poland",
			Country:      "Poland",
			CountryCode:  "pl",
		}
		model.CreateLocation(ctx, krakow)

		before := testing_countLocations(ctx)

		variants := []struct {
			countryCode string
			country     string
			region      string
			city        string
			want        *model.Location
		}{
			// accent dropped by the probing source
			{"BR", "Brazil", "Sao Paulo", "Sao Paulo", saoPaulo},
			{"CH", "Switzerland", "Zurich", "Zurich", zurich},
			// accent present in the probe, absent from the stored row
			{"PL", "Poland", "Lesser Poland", "Kraków", krakow},
			// accent on one level only
			{"BR", "Brazil", "São Paulo", "Sao Paulo", saoPaulo},
			// accent plus the other foldings at once
			{"CH", "Switzerland", "zurich", " ZURICH ", zurich},
		}
		for _, variant := range variants {
			label := fmt.Sprintf("%q/%q", variant.region, variant.city)
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

			_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
				ClientId:         clientId,
				CountryCode:      variant.countryCode,
				Country:          variant.country,
				Region:           variant.region,
				City:             variant.city,
				CountryConfident: true,
				CityConfident:    true,
				ObservedAt:       server.NowUtc(),
			})
			connect.AssertEqual(t, err, nil)

			stored := model.GetProviderEgressLocation(ctx, clientId)
			if stored == nil {
				t.Fatalf("%s: expected the submission to be stored", label)
			}
			if stored.LocationId != variant.want.LocationId {
				t.Errorf("%s resolved to %s, want the existing row %s", label, stored.LocationId, variant.want.LocationId)
			}
			if !stored.CityConfident {
				t.Errorf("%s: city_confident must stay set when the city resolved", label)
			}
		}

		if after := testing_countLocations(ctx); after != before {
			t.Errorf("location row count went from %d to %d; accented variants must reuse the existing row, not add new ones", before, after)
		}
	})
}

// The qualifier-stripping pass is the only one that can pick the wrong row, so
// it must decline rather than guess. Two same-region rows that differ ONLY in
// their parenthesised qualifier both reduce to the same key; a probe carrying a
// third qualifier matches neither exactly and must fall back to country instead
// of silently landing on whichever row the candidate ordering put first.
func TestSubmitProviderEgressLocationAmbiguousQualifierFallsBackToCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		springfieldIl := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Springfield (IL)",
			Region:       "Midwest",
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, springfieldIl)

		springfieldMa := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Springfield (MA)",
			Region:       "Midwest",
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, springfieldMa)

		before := testing_countLocations(ctx)

		networkId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		_, err := SubmitProviderEgressLocation(ctx, &SubmitProviderEgressLocationArgs{
			ClientId:         clientId,
			CountryCode:      "US",
			Country:          "United States",
			Region:           "Midwest",
			City:             "Springfield (OH)",
			CountryConfident: true,
			CityConfident:    true,
			ObservedAt:       server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)

		stored := model.GetProviderEgressLocation(ctx, clientId)
		if stored == nil {
			t.Fatal("expected the submission to be stored")
		}
		if stored.CityConfident {
			t.Errorf("an ambiguous qualifier must not resolve to a city; got city_confident with location %s", stored.LocationId)
		}
		if stored.LocationId == springfieldIl.LocationId || stored.LocationId == springfieldMa.LocationId {
			t.Errorf("guessed a Springfield (%s) instead of falling back to country", stored.LocationId)
		}
		// the country fallback goes through CreateLocation, but the US country
		// row already exists (the two Springfield fixtures created it), and
		// CreateLocation dedupes a country on country_code -- so declining to
		// guess must add no rows at all, least of all a third Springfield
		if after := testing_countLocations(ctx); after != before {
			t.Errorf("location row count went from %d to %d; the fallback must reuse the existing country row and never create a city", before, after)
		}
	})
}
