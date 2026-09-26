package egresshealth

import (
	"fmt"
	"strings"
)

// Places: where a provider is published and where a destination is known not
// to work from, and the split of a table for one provider's place.

// A country, or a region of one: where a provider is published, and
// where a destination is known not to work from (Destination.Incompatible).
//
// Country is a lower-case ISO 3166-1 alpha-2 code. Region is a region name as
// the server's places carry it, optional: an Incompatible entry without one
// means the whole country.
type Place struct {
	Country string `json:"country"`
	Region  string `json:"region,omitempty"`
}

// Reports whether an Incompatible entry applies to a provider at
// place: the same country, and either no region (the whole country) or the
// same region. Comparison ignores case and surrounding space -- both sides are
// data from the server, spelled by different writers -- and a provider whose
// country is unknown is covered by nothing, so a missing place can only widen
// its sample, never empty it.
func (self Place) covers(place Place) bool {
	country := strings.TrimSpace(self.Country)
	if country == "" || !strings.EqualFold(country, strings.TrimSpace(place.Country)) {
		return false
	}
	region := strings.TrimSpace(self.Region)
	return region == "" || strings.EqualFold(region, strings.TrimSpace(place.Region))
}

// Reports why an Incompatible entry cannot be applied, or nil.
func (self Place) valid() error {
	country := strings.TrimSpace(self.Country)
	if len(country) != 2 || country != strings.ToLower(country) {
		return fmt.Errorf("incompatible place %+v: the country must be a lower-case ISO 3166-1 alpha-2 code", self)
	}
	for _, c := range country {
		if c < 'a' || 'z' < c {
			return fmt.Errorf("incompatible place %+v: the country must be a lower-case ISO 3166-1 alpha-2 code", self)
		}
	}
	return nil
}

// Reports whether the destination is known not to work from
// place, so a provider published there must not be charged with it.
func (self Destination) IncompatibleWith(place Place) bool {
	for _, p := range self.Incompatible {
		if p.covers(place) {
			return true
		}
	}
	return false
}

// Splits a table for one provider's place: the destinations its
// sample may draw from, and the canaries it loads besides.
//
// A destination incompatible with the place is left out of the sample: a
// site blocked in a country says nothing about that country's exits, and
// charging them with it would make the country's whole fleet read as degraded
// (GEOMAP §11.3). The sample sizes are then met from what is left, never
// padded from the incompatible sites; a class too thin to fill its size is
// reported short (see Result.ShortClasses) as the pool's signal, not the provider's.
//
// An incompatible destination the server has marked Canary is loaded anyway,
// in addition to the sample and never scored: it is how the server learns the
// site works there again (§11.4). A canary-marked destination compatible with
// the place is an ordinary load.
func forPlace(table []Destination, place Place) (compatible []Destination, canaries []Destination) {
	compatible = make([]Destination, 0, len(table))
	for _, d := range table {
		switch {
		case !d.IncompatibleWith(place):
			compatible = append(compatible, d)
		case d.Canary:
			canaries = append(canaries, d)
		}
	}
	return compatible, canaries
}
