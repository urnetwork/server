// Tail markers prove client dispatch/return only. They neither name an SQL
// operation at deadline nor replace completed-frame/full-result authority.
package monitor

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"
)

// One fixed last boundary; no raw marker, nonce, target, error or SQL survives.
type pgSampleTailBoundary struct {
	Schema  int    `json:"schema"`
	Status  string `json:"status"`
	Phase   string `json:"phase"`
	Edge    string `json:"edge"`
	Index   *int   `json:"sample_index"`
	Records int    `json:"records"`
}

// The order is code-owned and bounded: two final waits/clears/activity queries,
// followed by the blockers clear/query and final history query.
func pgSampleTailSequence() string { return "wWcCaAwWcCaAcCbBhH" }

// Each matching record is exactly46 bytes. Existing versions1/2 ignore this
// prefix and retain their existing17 records; the combined healthy stream is
// 2624 bytes, below the unchanged4KiB cap. Foreign nonces consume no position.
func parsePgSampleTailBoundary(stderr, nonce string) *pgSampleTailBoundary {
	result := &pgSampleTailBoundary{Schema: 1, Status: "absent", Phase: "unknown", Edge: "unknown"}
	invalid := func(status string) *pgSampleTailBoundary {
		return &pgSampleTailBoundary{Schema: 1, Status: status, Phase: "unknown", Edge: "unknown"}
	}
	if len(stderr) > 4*1024 {
		return invalid("capped")
	}
	for _, line := range strings.SplitAfter(stderr, "\n") {
		if !strings.HasPrefix(line, "mpg3 ") {
			continue
		}
		if len(line) < 37 || line[5:37] != nonce {
			continue
		}
		n := result.Records
		if n >= 18 {
			return invalid("invalid")
		}
		code := pgSampleTailSequence()[n]
		index := 10
		if n >= 6 {
			index = 11
		}
		if n >= 12 {
			index = 12
		}
		if line != fmt.Sprintf("mpg3 %s %02d %c %02d\n", nonce, n, code, index) {
			return invalid("invalid")
		}
		phase, edge := "unknown", "entered"
		switch code {
		case 'w', 'W':
			phase = "sample_wait"
		case 'c', 'C':
			phase = "snapshot_clear"
		case 'a', 'A':
			phase = "activity"
		case 'b', 'B':
			phase = "blockers"
		case 'h', 'H':
			phase = "history_end"
		}
		if code >= 'A' && code <= 'Z' {
			edge = "returned"
		}
		result.Status, result.Phase, result.Edge = "valid", phase, edge
		result.Index, result.Records = &index, n+1
	}
	return result
}

// Absolute event times are private RAM only. Each exec-owned copy goroutine
// writes its own buffer; the caller reads it only after Cmd.Wait joins copies.
// ReaderFrom records actual EOF, never inferring it from a successful Wait.
type pgSampleNativeBuffer struct {
	bytes.Buffer
	eofAt time.Time
}

// Preserve the byte result/error of the existing owned pipe copy. No clock or
// receipt is shared with the other pipe while this method is running.
func (self *pgSampleNativeBuffer) ReadFrom(reader io.Reader) (int64, error) {
	var count int64
	buffer := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			written, _ := self.Write(buffer[:n])
			count += int64(written)
		}
		if err == io.EOF {
			self.eofAt = time.Now()
			return count, nil
		}
		if err != nil {
			return count, err
		}
	}
}

// Same-attempt local offsets only. EOF refers to local SSH pipe EOF, not a
// remote child or database completion. Overrides/preflight remain absent.
type pgSampleNativeLifecycle struct {
	Schema          int    `json:"schema"`
	ClockStatus     string `json:"clock_status"`
	StartEnteredNs  *int64 `json:"start_entered_ns"`
	StartReturnedNs *int64 `json:"start_returned_ns"`
	WaitEnteredNs   *int64 `json:"wait_entered_ns"`
	WaitReturnedNs  *int64 `json:"wait_returned_ns"`
	StdoutEofNs     *int64 `json:"stdout_eof_ns"`
	StderrEofNs     *int64 `json:"stderr_eof_ns"`
}

// Concurrent EOF clocks may precede Wait return and have no cross-pipe order.
// Invalid clocks withhold the entire timeline without changing process facts.
func (self *pgSampleTransportObservation) recordNativeLifecycle(startEntered, startReturned, waitEntered, waitReturned time.Time, out, errOut *pgSampleNativeBuffer) {
	result := &pgSampleNativeLifecycle{Schema: 1, ClockStatus: "ok"}
	invalid := self.localOrigin.IsZero() || startEntered.IsZero() || startReturned.Before(startEntered)
	point := func(value time.Time) *int64 {
		if value.IsZero() {
			return nil
		}
		n := value.Sub(self.localOrigin).Nanoseconds()
		if n < 0 {
			invalid = true
			return nil
		}
		return &n
	}
	result.StartEnteredNs, result.StartReturnedNs = point(startEntered), point(startReturned)
	result.WaitEnteredNs, result.WaitReturnedNs = point(waitEntered), point(waitReturned)
	result.StdoutEofNs, result.StderrEofNs = point(out.eofAt), point(errOut.eofAt)
	if !waitEntered.IsZero() && (waitEntered.Before(startReturned) || waitReturned.Before(waitEntered)) {
		invalid = true
	}
	for _, end := range []time.Time{out.eofAt, errOut.eofAt} {
		if !end.IsZero() && (end.Before(startEntered) || waitReturned.IsZero() || end.After(waitReturned)) {
			invalid = true
		}
	}
	if invalid {
		result = &pgSampleNativeLifecycle{Schema: 1, ClockStatus: "invalid"}
	}
	self.receipt.NativeLifecycle = result
}
