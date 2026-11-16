// What a validator's reserved upload is answered while this instance has no
// current authority, through the production route, JWT state and refresh loop.
package handlers

import (
	"bytes"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// The lane a test turns on, as a retry that succeeds turns on a process's lane.
type snReservedAttemptTestSwitchedLane struct {
	admission atomic.Pointer[controller.StReservedAttemptUpload]
}

func (self *snReservedAttemptTestSwitchedLane) Admission() (*controller.StReservedAttemptUpload, bool) {
	admission := self.admission.Load()
	return admission, admission == nil
}

// The route asks the lane on every request. A route that captured the lane when
// it was built could not serve a lane that came up later without a restart.
func TestSnReservedAttemptUploadLaneComesUpWithoutARestart(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		lane := &snReservedAttemptTestSwitchedLane{}
		endpoint := newSnReservedAttemptEndpointWithLane(tb, authority, 1, lane)
		data := []byte("{\"lane\":\"off\"}\n")
		header := snReservedAttemptIntentHeader(tb, authority, endpoint, data)
		status, body := snReservedAttemptResponse(tb, endpoint.origin, endpoint.api.GetByJwt(), header, "metadata", data)
		requireDeclaredAnswer(tb, status, body, http.StatusServiceUnavailable, "Validator reserved upload is not running on this instance.")
		if endpoint.reads.Load() != 0 || endpoint.stores.Load() != 0 {
			tb.Fatal("a lane that is off reached body or storage")
		}
		// The validator's own writer sees the status its retry owners act on.
		if err := endpoint.writer.Write(tb.Context(), "metadata", snAttemptTestHash(data), data); err == nil || !strings.Contains(err.Error(), "status is 503") {
			tb.Fatalf("the validator's writer while the lane is off: %v", err)
		}
		lane.admission.Store(authority.start(tb, 1))
		if err := endpoint.writer.Write(tb.Context(), "metadata", snAttemptTestHash(data), data); err != nil {
			tb.Fatalf("the same route after the lane came up: %v", err)
		}
		actual, err := endpoint.reader.ReadMetadata(tb.Context(), snAttemptTestHash(data), uint64(len(data)))
		if err != nil || !bytes.Equal(actual, data) {
			tb.Fatalf("the stored object differs: %v", err)
		}
	})
}
