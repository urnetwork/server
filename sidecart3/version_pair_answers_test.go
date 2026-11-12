package sidecart3

// What version_pair.go's result reads as a problem, with no store or process: every wrong answer, and no right one.

import (
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
)

// A wrong answer is a problem: a planted type answered anything but UNIMPLEMENTED, a planted major anything but VERSION
// with the receiver's range, and a silent drop.
func TestVersionPairProblemsNameEveryWrongAnswer(t *testing.T) {
	result := &VersionPairResult{Answers: []*VersionPairAnswer{
		{From: "N+1", To: "N", MessageType: 0x8f01, Code: sidecarprotocol.ErrorCode_ERR_UNIMPLEMENTED},
		{From: "N+1", To: "N", MessageType: 0x8f02, Code: sidecarprotocol.ErrorCode_ERR_INVALID_ARGUMENT},
		{From: "N+1", To: "N", MessageType: sidecarprotocol.MessageType_PING, PlantedMajor: true, WireVersion: 2, Code: sidecarprotocol.ErrorCode_ERR_VERSION, MinVersion: sidecar.WireVersionMajor, MaxVersion: sidecar.WireVersionMajor},
		{From: "N", To: "N+1", MessageType: sidecarprotocol.MessageType_PING, PlantedMajor: true, WireVersion: 0, Code: sidecarprotocol.ErrorCode_ERR_VERSION},
		{From: "N", To: "N+1", MessageType: sidecarprotocol.MessageType_PING, PlantedMajor: true, WireVersion: 9, Code: sidecarprotocol.ErrorCode_ERR_UNIMPLEMENTED, MinVersion: sidecar.WireVersionMajor, MaxVersion: sidecar.WireVersionMajor},
		{From: "N", To: "N+1", MessageType: 0x8f7f, Err: errSilentDropForTest},
	}}
	problems := result.Problems()
	if len(problems) != 4 {
		t.Fatalf("problems %q", problems)
	}
	for index, want := range []string{"0x8f02: answered ERR_INVALID_ARGUMENT", "wire major 0: VERSION named 0 to 0", "wire major 9: answered ERR_UNIMPLEMENTED", "0x8f7f: no answer"} {
		if !strings.Contains(problems[index], want) {
			t.Errorf("problem %d reads %q, not %q", index, problems[index], want)
		}
	}
}

// What a planted type's call ended with when no answer came.
var errSilentDropForTest = errors.New("no answer")
