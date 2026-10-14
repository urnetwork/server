// Test support for a package whose config home has no GeoLite2 City database.
package server

import (
	"testing"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

// Makes GetIpInfo read the GeoLite2 City database at path for the rest of a
// serial test, whatever the config home holds, and puts the process's reader
// back afterwards. The file is opened as the runtime opens the deployed one.
// Install it before anything that looks an address up starts. The stand-in
// stays open for the life of the process, so a lookup still in flight when
// the test ends reads a mapped file.
func UseIpDbFileForTest(t testing.TB, path string) {
	t.Helper()
	var db *mmdb.Reader
	var databaseType schemaType
	if r := HandleError(func() { db, databaseType = openIpDb(path) }); r != nil {
		t.Fatal(r)
	}
	previous := ipDb
	ipDb = func() (*mmdb.Reader, schemaType) {
		return db, databaseType
	}
	t.Cleanup(func() {
		ipDb = previous
	})
}
