package model

import (
	"net/netip"

	"github.com/urnetwork/connect/lagdrift"
)

// How far an extender's place may be off, s_ext in the drift model's variance
// (lag.dev/DRIFTTEST.md V3): GeoLite2's accuracy radius for the address the
// place was taken from, held to [10, 100] km, or 50 km for a place moved to
// the extender's derived location (A4), as lagdrift.HeldPlaceErrorKm holds it.

// One extender as the drift test places it: where the sample put it
// (GetLagExtenderPlaces) and the addresses it was placed by.
type LagDriftPlacedExtender struct {
	Place *LagPlace
	Ips   []netip.Addr
}

// The place error of each extender in km, in order (V3). The place is the one
// GeoLite2 gives an address of the extender when one of its addresses has
// exactly that point, and the error is then that address's accuracy radius,
// held; an unknown radius is held to 50 km. Otherwise the dot was moved to the
// extender's derived location, which is trusted to 50 km
// (LagExtenderDerivedPlaceMaxResidualKm). GeoLite2 is read once for every
// address, through the lookup the sample's places come from
// (lagAddressPlaces), so a test's stand-in answers both.
func GetLagDriftExtenderPlaceErrorsKm(extenders []*LagDriftPlacedExtender) []float64 {
	ips := []netip.Addr{}
	for _, extender := range extenders {
		for _, ip := range extender.Ips {
			ips = append(ips, ip.Unmap().WithZone(""))
		}
	}
	addressPlaces := lagAddressPlaces(ips)

	placeErrorsKm := make([]float64, 0, len(extenders))
	for _, extender := range extenders {
		placeErrorKm := float64(lagdrift.DerivedPlaceErrorKm)
		for _, ip := range extender.Ips {
			addressPlace := addressPlaces[ip.Unmap().WithZone("")]
			if addressPlace == nil || extender.Place == nil {
				continue
			}
			if addressPlace.Place.Latitude == extender.Place.Latitude && addressPlace.Place.Longitude == extender.Place.Longitude {
				placeErrorKm = lagdrift.HeldPlaceErrorKm(float64(addressPlace.AccuracyRadiusKm))
				break
			}
		}
		placeErrorsKm = append(placeErrorsKm, placeErrorKm)
	}
	return placeErrorsKm
}
