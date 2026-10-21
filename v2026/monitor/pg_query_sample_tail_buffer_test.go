// Exercise the actual embedded reader function with fixed chunks. No fixture
// SQL, timing, process scheduler or fabricated transport parser proves these
// byte-buffer invariants; this is a synthetic reader-function control only.
package monitor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Use the production definitions byte-for-byte, stopping before input/launch.
// The actual EOF path is separately covered through the complete reader below.
func pgTailBufferFixture(t *testing.T, payload string, chunk int) (string, string) {
	t.Helper()
	const start = "\ntry:\n cfg="
	if strings.Count(pgQuerySampleProgram, start) != 1 {
		t.Fatal("reader launch boundary changed")
	}
	program := strings.SplitN(pgQuerySampleProgram, start, 2)[0]
	program += "\nnonce='" + pgTransportFixtureNonce + "'\noutput=bytearray()\nprogress('remote_started',0)\npayload=sys.stdin.buffer.read(4194306)\ncapped=False\ntry:\n"
	program += fmt.Sprintf(" for offset in range(0,len(payload),%d):absorb_stdout(payload[offset:offset+%d])\n", chunk, chunk)
	program += "except Failure as err:\n if err.phase!='output' or err.cause!='output_cap':raise\n capped=True\n"
	program += "expected=payload\nfor sequence,(code,index) in enumerate(tail_expected):expected=expected.replace(('monitor-pg-tail %s %02d %s %02d\\n'%(nonce,sequence,code,index)).encode(),b'',1)\n"
	program += "sys.stdout.write(json.dumps({'capped':capped,'charged':stdout_received,'pending':len(stdout_pending),'records':tail_sequence,'equal':bytes(output)==expected},separators=(',',':')))\n"
	out, stderr, err := runPgSampleProgramOutputFixture(t, program, "", payload)
	if err != nil {
		t.Fatal("actual reader function control did not return")
	}
	return out, stderr
}

// Byte-at-a-time and one coalesced call cross every marker/JSON separator
// deterministically. The real function must remove only exact marker bytes.
func TestPgTailBufferSplitAndCoalescedActualFunction(t *testing.T) {
	payload, _ := pgTailFixtureOutput(t, 18)
	for _, chunk := range []int{1, len(payload)} {
		out, stderr := pgTailBufferFixture(t, payload, chunk)
		var result struct {
			Capped  bool `json:"capped"`
			Charged int  `json:"charged"`
			Pending int  `json:"pending"`
			Records int  `json:"records"`
			Equal   bool `json:"equal"`
		}
		if json.Unmarshal([]byte(out), &result) != nil || result.Capped || result.Charged != len(payload) || result.Pending != 0 || result.Records != 18 || !result.Equal {
			t.Fatal("reader chunk boundaries changed stripped source bytes")
		}
		old := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
		tail := parsePgSampleTailBoundary(stderr, pgTransportFixtureNonce)
		if old.Frames == nil || *old.Frames != 12 || tail.Records != 18 || tail.Edge != "returned" || len(stderr) != 2624 {
			t.Fatal("fragmented markers changed progress or its cap")
		}
	}
}

// Exactly the existing raw byte cap succeeds, while its next byte refuses.
// Removing markers from accounting would incorrectly let the second pass.
func TestPgTailBufferRawCapChargesMarkerBytes(t *testing.T) {
	payload, _ := pgTailFixtureOutput(t, 18)
	const capBytes = 4 * 1024 * 1024
	if len(payload) >= capBytes {
		t.Fatal("synthetic source exceeds control cap")
	}
	payload += strings.Repeat(" ", capBytes-len(payload)-1) + "\n"
	for _, extra := range []string{"", " "} {
		out, stderr := pgTailBufferFixture(t, payload+extra, 65536)
		var result struct {
			Capped  bool `json:"capped"`
			Charged int  `json:"charged"`
			Records int  `json:"records"`
			Equal   bool `json:"equal"`
		}
		if json.Unmarshal([]byte(out), &result) != nil || result.Capped != (extra != "") || result.Charged != capBytes+len(extra) || result.Records != 18 || (extra == "" && !result.Equal) {
			t.Fatal("raw output cap excluded marker overhead or changed exact boundary")
		}
		if tail := parsePgSampleTailBoundary(stderr, pgTransportFixtureNonce); tail.Status != "valid" || tail.Records != 18 {
			t.Fatal("cap control invented unknown prior marker progress")
		}
	}
}

// EOF with an incomplete marker must preserve those bytes for full-parser
// refusal and cannot promote an unreceived entry from the previous prefix.
func TestPgTailBufferTruncatedMarkerActualEof(t *testing.T) {
	prefix, _ := pgTailFixtureOutput(t, 0)
	line := strings.Replace(pgTailFixtureLine(pgTransportFixtureNonce, 0), "mpg3 ", "monitor-pg-tail ", 1)
	payload := prefix + strings.TrimSuffix(line, "\n")
	quoted, _ := json.Marshal(payload)
	const read = "chunk=os.read(key.fileobj.fileno(),65536)"
	if strings.Count(pgQuerySampleProgram, read) != 1 {
		t.Fatal("reader pipe read boundary changed")
	}
	program := strings.Replace(pgQuerySampleProgram, read, "chunk=os.read(key.fileobj.fileno(),1)", 1)
	out, stderr, err := runPgSampleProgramOutputFixture(t, program, "sys.stdin.read()\nsys.stdout.write("+string(quoted)+")\n", pgSampleProgramInput(t))
	if err != nil || out != payload {
		t.Fatal("actual pipe EOF discarded its partial marker bytes")
	}
	_, parseErr := parsePgQuerySample(out, time.Now())
	tail := parsePgSampleTailBoundary(stderr, pgTransportFixtureNonce)
	old := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
	if parseErr == nil || tail.Status != "absent" || tail.Index != nil || old.Frames == nil || *old.Frames != 10 {
		t.Fatal("truncated EOF acquired false healthy or entered authority")
	}
}
