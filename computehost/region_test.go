package computehost

// The broker's region lookup (CARCH-M12-01, DESIGN-COMPUTE §17.2, I-13): the process's GeoLite2 City reader answers a
// documentation address as placed nowhere, or, in a process with no database, an error by which the broker refuses
// every admission; neither the region nor the error carries the address asked.

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Documentation addresses, which no database places, answer a zero region where the database is deployed and an error
// where it is not; no answer repeats the address.
func TestHostRegionLookupKeepsTheAddressOutOfItsAnswers(t *testing.T) {
	for _, text := range []string{"203.0.113.77", "2001:db8:5:6::77"} {
		addr := netip.MustParseAddr(text)
		region, err := hostRegionLookup{}.Region(addr)
		switch {
		case !server.IpInfoDatabaseDeployed():
			if err == nil || region != nil {
				t.Fatalf("a process with no database answered %+v %v", region, err)
			}
		case err != nil:
			t.Fatalf("%s: %v", text, err)
		case region == nil || region.CountryCode != "" || region.SubdivisionGeonameId != 0:
			t.Fatalf("a documentation address was placed: %+v", region)
		}
		if err != nil && strings.Contains(err.Error(), text) {
			t.Fatalf("the error repeats the address: %v", err)
		}
	}
}
