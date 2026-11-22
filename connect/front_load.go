package connect

// What a connect block's lb_alt front is built from (LBALT-IMPL.md §2.2,
// §2.8). warpctl renders the route file for one connect generation, mounts it
// read-only and names it in WARP_LBALT_ROUTE_FILE, with the generation in
// WARP_GENERATION. Connect loads the file and every certificate it serves
// before it can report ready, and fails its start on any error, so it never
// comes up half-configured. Without WARP_LBALT_ROUTE_FILE, connect has no
// front and nothing changes.
//
// The vault's lbalt_session_tickets.yml, when it sets enabled, gives the
// secrets of the session ticket keys every front shares (LBALTOPT2 R22 (a);
// nginx/tls_ticket.go). Without the file, or with enabled false, each block
// keeps Go's per-process keys (LBALT.md D6c). A file that sets enabled with
// secrets the front can't use fails the start, as a certificate does.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"

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
	settings := DefaultFrontServerSettings()
	sessionTicketSecrets, err := loadFrontSessionTicketSecrets()
	if err != nil {
		return nil, err
	}
	if 0 < len(sessionTicketSecrets) {
		glog.Infof("[front]session ticket keys shared from %d secrets of the vault's %s\n", len(sessionTicketSecrets), frontSessionTicketsResource)
	}
	settings.Front.Tls.SessionTicketSecrets = sessionTicketSecrets
	return newFrontServer(load, settings)
}

// The vault resource that turns the shared session ticket keys on
// (LBALTOPT2 R22 (a)):
//
//	enabled: true
//	secrets:
//	  - <base64 of at least 32 random bytes: derives the key that encrypts>
//	  - <an earlier secret, while a change rolls through the blocks>
const frontSessionTicketsResource = "lbalt_session_tickets.yml"

// The largest lbalt_session_tickets.yml read.
const frontSessionTicketsMaxBytes = 4096

// The secrets of the shared session ticket keys from the vault's
// lbalt_session_tickets.yml, in its order, or nil, which keeps Go's
// per-process keys: without the file, or with enabled false. A file that
// sets enabled must list between one and nginx.TlsTicketSecretMaxCount
// standard base64 secrets of at least nginx.TlsTicketSecretMinBytes each,
// and name no other field. No error quotes the file's content.
func loadFrontSessionTicketSecrets() ([][]byte, error) {
	resource, err := server.Vault.SimpleResource(frontSessionTicketsResource)
	if errors.Is(err, server.ErrResourceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", frontSessionTicketsResource, err)
	}
	raw, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("%s: unreadable", frontSessionTicketsResource)
	}
	return parseFrontSessionTicketSecrets(raw)
}

// The secrets of an lbalt_session_tickets.yml's content, as
// loadFrontSessionTicketSecrets reads them.
func parseFrontSessionTicketSecrets(raw []byte) ([][]byte, error) {
	if frontSessionTicketsMaxBytes < len(raw) {
		return nil, fmt.Errorf("%s: over %d bytes", frontSessionTicketsResource, frontSessionTicketsMaxBytes)
	}
	var config struct {
		Enabled bool     `yaml:"enabled"`
		Secrets []string `yaml:"secrets"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil && err != io.EOF {
		// a yaml error can quote the content
		return nil, fmt.Errorf("%s: not the expected fields", frontSessionTicketsResource)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%s: more than one document", frontSessionTicketsResource)
	}
	if !config.Enabled {
		return nil, nil
	}
	if len(config.Secrets) == 0 {
		return nil, fmt.Errorf("%s: enabled without secrets", frontSessionTicketsResource)
	}
	if nginx.TlsTicketSecretMaxCount < len(config.Secrets) {
		return nil, fmt.Errorf("%s: more than %d secrets", frontSessionTicketsResource, nginx.TlsTicketSecretMaxCount)
	}
	secrets := make([][]byte, 0, len(config.Secrets))
	for i, encodedSecret := range config.Secrets {
		secret, err := base64.StdEncoding.DecodeString(encodedSecret)
		if err != nil {
			return nil, fmt.Errorf("%s: secret %d is not standard base64", frontSessionTicketsResource, i)
		}
		if len(secret) < nginx.TlsTicketSecretMinBytes {
			return nil, fmt.Errorf("%s: secret %d is shorter than %d bytes", frontSessionTicketsResource, i, nginx.TlsTicketSecretMinBytes)
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
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
