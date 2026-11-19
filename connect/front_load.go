package connect

// What a connect block's lb_alt front is built from (LBALT-IMPL.md §2.2,
// §2.8). warpctl renders the route file for one connect generation, mounts it
// read-only and names it in WARP_LBALT_ROUTE_FILE, with the generation in
// WARP_GENERATION. Connect loads the file and every certificate it serves
// before it can report ready, and fails its start on any error, so it never
// comes up half-configured. Without WARP_LBALT_ROUTE_FILE, connect has no
// front and nothing changes.

import (
	"fmt"
	"os"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/nginx"
)

// The environment warpctl gives a connect block that fronts its interfaces.
const (
	// the route file's path inside the container
	frontRouteFileEnvName = "WARP_LBALT_ROUTE_FILE"
	// this generation's container name, which the route file also carries
	frontGenerationEnvName = "WARP_GENERATION"
)

// A loaded route file and its certificates.
type frontLoad struct {
	routeTable *nginx.RouteTable
	// the lowercase hex sha256 of the route file's bytes
	revision string
	// WARP_GENERATION, which the route file agrees with
	generation       string
	certificateStore *nginx.CertificateStore
	// the earliest expiry among the certificates
	certificateExpiry time.Time
}

// The front server of this block, loaded and with its sockets registered,
// or nil without WARP_LBALT_ROUTE_FILE.
func newFrontServerFromEnv() (*frontServer, error) {
	routeFilePath := os.Getenv(frontRouteFileEnvName)
	if routeFilePath == "" {
		return nil, nil
	}
	env, err := server.Env()
	if err != nil {
		return nil, err
	}
	load, err := loadFront(routeFilePath, os.Getenv(frontGenerationEnvName), env, server.VaultHomeRoot())
	if err != nil {
		return nil, err
	}
	return newFrontServer(load, DefaultFrontServerSettings())
}

// Loads the route file at routeFilePath and the certificate of every name it
// serves from the vault at vaultDir. The file must be this generation's and
// this env's: a mismatch means warpctl mounted another block's file. An empty
// generation takes the file's.
func loadFront(routeFilePath string, generation string, env string, vaultDir string) (*frontLoad, error) {
	routeTable, revision, err := nginx.LoadRouteTable(routeFilePath)
	if err != nil {
		return nil, err
	}
	if generation == "" {
		generation = routeTable.Generation
	} else if generation != routeTable.Generation {
		return nil, fmt.Errorf("route file is generation %s, not %s", routeTable.Generation, generation)
	}
	if routeTable.Env != env {
		return nil, fmt.Errorf("route file is env %s, not %s", routeTable.Env, env)
	}
	certificateStore, err := nginx.LoadCertificateStore(vaultDir, env, routeTable)
	if err != nil {
		return nil, err
	}
	return &frontLoad{
		routeTable:        routeTable,
		revision:          revision,
		generation:        generation,
		certificateStore:  certificateStore,
		certificateExpiry: certificateStore.EarliestExpiry(),
	}, nil
}
