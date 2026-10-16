package model

import (
	"path/filepath"
	"testing"

	"github.com/urnetwork/server"
)

// Makes the GeoLite2 City lookup read the repository's synthetic empty
// database for the rest of the test. A test that connects from a reserved
// address no ip override covers (198.51.100.0/24 and 203.0.113.0/24 of RFC
// 5737, or a private range of RFC 1918) otherwise reads the config home's
// database, which a portable fixture does not have. The synthetic one knows
// no address, so each such address is unknown geography, as the deployed
// build leaves it.
func useSyntheticIpDb(t testing.TB) {
	t.Helper()
	server.UseIpDbFileForTest(t, filepath.Join("..", "local", "testdata", "config", "local", "mmdb", "geolite2.mmdb"))
}
