package model

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
)

// device.client_version holds at most this many bytes
const deviceClientVersionMaxLen = 64

// the process remembers this many clients' last written version before it
// forgets them all and starts again
const deviceClientVersionCacheMax = 1 << 16

// The version write is best effort on the connect path, so it waits for a
// busy device row only briefly.
const deviceClientVersionLockTimeout = 500 * time.Millisecond

var deviceClientVersionCache = struct {
	lock     sync.Mutex
	versions map[server.Id]string
}{
	versions: map[server.Id]string{},
}

// Records the app version a client's device connected with: the connect auth's
// app_version. A client with no device, or an empty version, writes nothing.
// The row is written only when the version changes, and this process skips
// clients whose version it already wrote, so reconnects cost no write.
func SetDeviceClientVersion(ctx context.Context, clientId server.Id, clientVersion string) {
	clientVersion = strings.TrimSpace(clientVersion)
	if clientVersion == "" {
		return
	}
	if deviceClientVersionMaxLen < len(clientVersion) {
		clientVersion = clientVersion[:deviceClientVersionMaxLen]
	}

	deviceClientVersionCache.lock.Lock()
	written, ok := deviceClientVersionCache.versions[clientId]
	deviceClientVersionCache.lock.Unlock()
	if ok && written == clientVersion {
		return
	}

	// a bounded wait, so the write never pins a pooled server behind the
	// device row's lockers (an auth or registration of the device's clients);
	// a write they hold off is not remembered, so a later connection retries
	timedOut := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); ok && server.IsLockTimeout(err) {
					timedOut = true
					return
				}
				panic(r)
			}
		}()
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := tx.Exec(
				ctx,
				`
					UPDATE device
					SET
						client_version = $2,
						client_version_time = $3
					FROM network_client
					WHERE
						network_client.client_id = $1 AND
						device.device_id = network_client.device_id AND
						device.client_version IS DISTINCT FROM $2
				`,
				clientId,
				clientVersion,
				server.NowUtc(),
			)
			server.Raise(err)
		}, server.TxReadCommitted, server.TxLockTimeout(deviceClientVersionLockTimeout))
	}()
	if timedOut {
		return
	}

	deviceClientVersionCache.lock.Lock()
	defer deviceClientVersionCache.lock.Unlock()
	if deviceClientVersionCacheMax <= len(deviceClientVersionCache.versions) {
		clear(deviceClientVersionCache.versions)
	}
	deviceClientVersionCache.versions[clientId] = clientVersion
}
