// Independent wire regression: the existing join projector must retain the
// fixed received tail boundary without inventing native process authority.
package monitor

import (
	"encoding/json"
	"strings"
	"testing"
)

// This tests-only RED file compiles against the old observer and does not
// reference a proposed production type or new helper. No process is claimed.
func TestPgTailBoundaryJoinedPartialMarkerHasTypedProjection(t *testing.T) {
	const nonce = "00112233445566778899aabbccddeeff"
	stderr := pgCompletedProgressStream(nonce, 10, false) + "mpg3 " + nonce + " 00 w 10\n"
	observation := &pgSampleTransportObservation{nonce: nonce}
	observation.joined(nil, stderr)
	raw, err := json.Marshal(observation.receipt)
	var wire struct {
		Frames *int `json:"remote_sample_frames_observed"`
		Exit   *int `json:"native_exit_code"`
		Tail   *struct {
			Status  string `json:"status"`
			Phase   string `json:"phase"`
			Edge    string `json:"edge"`
			Index   *int   `json:"sample_index"`
			Records int    `json:"records"`
		} `json:"tail_boundary"`
	}
	if err != nil || json.Unmarshal(raw, &wire) != nil {
		t.Fatal("observer receipt was not encoded")
	}
	if wire.Tail == nil || wire.Tail.Status != "valid" || wire.Tail.Phase != "sample_wait" || wire.Tail.Edge != "entered" || wire.Tail.Index == nil || *wire.Tail.Index != 10 || wire.Tail.Records != 1 {
		t.Fatal("joined fixed tail marker has no typed client-boundary projection")
	}
	if wire.Frames == nil || *wire.Frames != 10 || wire.Exit != nil || strings.Contains(string(raw), nonce) {
		t.Fatal("tail projection changed prior progress, native unknown or privacy")
	}
}

// Invalid-length correlation is not positive production-input authority. The
// valid fixture above exercises the unchanged exact32-character protocol.
func TestPgTailBoundaryInvalidNonceLengthsStayUnobserved(t *testing.T) {
	for _, length := range []int{31, 33, 34} {
		nonce := strings.Repeat("a", length)
		observation := &pgSampleTransportObservation{nonce: nonce}
		observation.joined(nil, "mpg3 "+nonce+" 00 w 10\n")
		raw, err := json.Marshal(observation.receipt)
		var wire struct {
			Tail *struct {
				Status  string `json:"status"`
				Index   *int   `json:"sample_index"`
				Records int    `json:"records"`
			} `json:"tail_boundary"`
		}
		if err != nil || json.Unmarshal(raw, &wire) != nil {
			t.Fatal("invalid correlation receipt did not encode")
		}
		if wire.Tail != nil && (wire.Tail.Status == "valid" || wire.Tail.Index != nil || wire.Tail.Records != 0) {
			t.Fatal("invalid correlation length acquired positive tail authority")
		}
	}
}
