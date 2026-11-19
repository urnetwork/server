package connect

// The front's readiness on connect's private /status (LBALT-IMPL.md §2.4,
// §4.7, D13). warpctl probes a candidate generation on its private port and
// switches the block's DNAT only when these headers show that generation, the
// revision of the route file it rendered, an explicit ready state and every
// expected socket up. X-UR-Connect-Listeners-Ready and
// X-UR-Connect-UDP-Listeners keep their meaning for the nginx path.
//
// Each socket counts on its own, so a group of four with one missing reads
// 3/4 and the front not ready until the socket is back (D22). Draining is
// never ready, whatever the sockets.

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/router"
)

// The readiness headers (LBALT-IMPL.md §2.4).
const (
	frontGenerationHeader    = "X-UR-Front-Generation"
	frontRouteRevisionHeader = "X-UR-Front-Route-Revision"
	frontReadyHeader         = "X-UR-Front-Ready"
	frontSocketsHeader       = "X-UR-Front-Sockets"
	frontCertExpiryHeader    = "X-UR-Front-Cert-Expiry"
)

// The status latch's value for a ready service (router.WarpStatusState).
const frontReadyLatchStatus = "ok"

// Writes the front's readiness headers. latchStatus is the service's status
// latch, and othersReady whether the rest of connect is ready; the front is
// ready only with both, every expected socket up, and no drain begun.
func (self *frontServer) writeStatusHeaders(header http.Header, latchStatus string, othersReady bool) {
	type groupCount struct {
		up       int
		expected int
	}
	groupCounts, socketsReady := func() (map[string]*groupCount, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		groupCounts := map[string]*groupCount{}
		allUp := true
		for _, socket := range self.sockets {
			groupName := socket.key.groupName()
			count, ok := groupCounts[groupName]
			if !ok {
				count = &groupCount{}
				groupCounts[groupName] = count
			}
			count.expected += 1
			if socket.up {
				count.up += 1
			} else {
				allUp = false
			}
		}
		return groupCounts, self.started && allUp && !self.draining && !self.closing
	}()

	entries := make([]string, 0, len(groupCounts))
	for groupName, count := range groupCounts {
		entries = append(entries, groupName+"="+strconv.Itoa(count.up)+"/"+strconv.Itoa(count.expected))
	}
	slices.Sort(entries)

	ready := "0"
	if othersReady && latchStatus == frontReadyLatchStatus && socketsReady {
		ready = "1"
	}
	header.Set(frontGenerationHeader, self.generation)
	header.Set(frontRouteRevisionHeader, self.revision)
	header.Set(frontReadyHeader, ready)
	header.Set(frontSocketsHeader, strings.Join(entries, ","))
	header.Set(frontCertExpiryHeader, self.certificateExpiry.UTC().Format(time.RFC3339))
}

// The private /status handler of a connect that loaded its route file but
// never got to serve, its startup readiness having failed: the front's
// headers, never ready, before the latch's json. Without a front, the latch's
// handler unchanged.
func frontStatusHandler(publicFront *frontServer, warpStatus *router.WarpStatusState) http.HandlerFunc {
	if publicFront == nil {
		return warpStatus.Handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		publicFront.writeStatusHeaders(w.Header(), warpStatus.Status(), false)
		warpStatus.Handler(w, r)
	}
}
