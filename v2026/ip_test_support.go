// Test support for a package whose config home has no GeoLite2 City database,
// or has ip overrides a test must not read.
package server

import (
	"fmt"
	"sync/atomic"
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
	previousDeployed := ipDbDeployed
	ipDb = func() (*mmdb.Reader, schemaType) {
		return db, databaseType
	}
	ipDbDeployed = func() bool {
		return true
	}
	t.Cleanup(func() {
		ipDb = previous
		ipDbDeployed = previousDeployed
	})
}

// Makes the process have no GeoLite2 City database for the rest of a serial
// test, as a process whose config home has none: IpInfoDatabaseDeployed
// answers false, and a lookup panics, as the runtime's does. Returns how many
// times a lookup read the missing database, so a test can assert that its
// caller did not try. Puts the process's reader back afterwards.
func UseNoIpDbForTest(t testing.TB) (reads func() int) {
	t.Helper()
	var readCount atomic.Int64
	previous := ipDb
	previousDeployed := ipDbDeployed
	ipDb = func() (*mmdb.Reader, schemaType) {
		readCount.Add(1)
		panic(fmt.Errorf("no GeoLite2 City database at %s (test)", GeoLite2CityResourcePath))
	}
	ipDbDeployed = func() bool {
		return false
	}
	t.Cleanup(func() {
		ipDb = previous
		ipDbDeployed = previousDeployed
	})
	return func() int {
		return int(readCount.Load())
	}
}

// Makes no ip_overrides subnet answer for the rest of a test, and puts the
// process's overrides back afterwards. Each address is then looked up in the
// databases, as on a deployment without overrides, so a test that asserts how
// the deployed build leaves a loopback or documentation address does not read
// a portable fixture's override instead. Pair it with UseIpDbFileForTest where
// the config home may have no GeoLite2 database. The overrides belong to the
// process, so the test cannot be parallel: the marker set through t.Setenv
// makes the testing package refuse t.Parallel, before this call or after it.
func UseNoIpOverridesForTest(t testing.TB) {
	t.Helper()
	t.Setenv("URNETWORK_TEST_NO_IP_OVERRIDES", "1")
	previous := ipOverrides
	ipOverrides = func() []*ipOverride {
		return []*ipOverride{}
	}
	t.Cleanup(func() {
		ipOverrides = previous
	})
}
