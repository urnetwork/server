package model


import (
    "context"
    "testing"

    "github.com/go-playground/assert/v2"

    "bringyour.com/bringyour"
)


func TestAddDefaultLocations(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    AddDefaultLocations(ctx, 10)
})}


func TestCanonicalLocations(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    us1 := &Location{
        LocationType: LocationTypeCountry,
        Country: "United States",
        CountryCode: "us",
    }
    CreateLocation(ctx, us1)

    assert.Equal(t, us1.LocationId, us1.CountryLocationId)

    us2 := &Location{
        LocationType: LocationTypeCountry,
        Country: "United States",
        CountryCode: "us",
    }
    CreateLocation(ctx, us2)

    assert.Equal(t, us2.LocationId, us1.LocationId)
    assert.Equal(t, us2.LocationId, us2.CountryLocationId)

    a := &Location{
        LocationType: LocationTypeRegion,
        Region: "California",
        Country: "United States",
        CountryCode: "us",
    }
    CreateLocation(ctx, a)

    assert.Equal(t, a.LocationId, a.RegionLocationId)
    assert.Equal(t, a.CountryLocationId, us1.LocationId)

    b := &Location{
        LocationType: LocationTypeRegion,
        Region: "California",
        Country: "United States",
        CountryCode: "us",
    }
    CreateLocation(ctx, b)

    assert.Equal(t, a.LocationId, b.LocationId)
    assert.Equal(t, a.RegionLocationId, b.RegionLocationId)
    assert.Equal(t, a.CountryLocationId, b.CountryLocationId)


    c := &Location{
        LocationType: LocationTypeCity,
        City: "Palo Alto",
        Region: "California",
        Country: "United States",
        CountryCode: "us",
    }
    CreateLocation(ctx, c)

    assert.Equal(t, c.RegionLocationId, a.LocationId)
    assert.Equal(t, c.CountryLocationId, a.CountryLocationId)

    d := &Location{
        LocationType: LocationTypeCity,
        City: "Palo Alto",
        Region: "California",
        Country: "United States",
        CountryCode: "us",
    }
    CreateLocation(ctx, d)

    assert.Equal(t, d.LocationId, c.LocationId)
    assert.Equal(t, d.RegionLocationId, c.RegionLocationId)
    assert.Equal(t, d.CountryLocationId, c.CountryLocationId)
})}


func TestCanonicalLocationsParallel(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    n := 1000
    out := make(chan bringyour.Id, n)

    for i := 0; i < n; i += 1 {
        go func() {
            c := &Location{
                LocationType: LocationTypeCity,
                City: "Palo Alto",
                Region: "California",
                Country: "United States",
                CountryCode: "us",
            }
            CreateLocation(ctx, c)
            out <- c.LocationId
        }()
    }

    locationIds := map[bringyour.Id]bool{}
    for i := 0; i < n; i += 1 {
        locationId := <- out
        locationIds[locationId] = true
    }

    assert.Equal(t, 1, len(locationIds))
})}

