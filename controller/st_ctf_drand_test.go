package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/ctf"
)

// a relay that serves the answer of answer(round) at quicknet's path
func ctfTestRelay(t *testing.T, answer func(round string) (int, string)) (*httptest.Server, *[]string) {
	t.Helper()
	asked := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round, ok := strings.CutPrefix(r.URL.Path, "/"+crv4.QuicknetChainHash+"/public/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		*asked = append(*asked, round)
		status, body := answer(round)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, asked
}

func ctfTestRelayBody(round uint64, signature string) string {
	raw, _ := hex.DecodeString(signature)
	randomness := sha256.Sum256(raw)
	return fmt.Sprintf(`{"round":%d,"randomness":"%x","signature":"%s"}`, round, randomness, signature)
}

func resetCtfPulses(t *testing.T) {
	stCtfPulses.Clear()
	t.Cleanup(func() { stCtfPulses.Clear() })
}

// §4.4: a pulse is kept only once its BLS signature verifies against
// quicknet's key; a relay that serves anything else is passed over for the
// next, and a verified pulse is kept for the process.
func TestStCtfDrandPulseVerifies(t *testing.T) {
	resetCtfPulses(t)
	good, goodAsked := ctfTestRelay(t, func(round string) (int, string) {
		return http.StatusOK, ctfTestRelayBody(1000, ctfTestPulses[1000])
	})
	for name, answer := range map[string]func(string) (int, string){
		"another round":     func(string) (int, string) { return http.StatusOK, ctfTestRelayBody(1001, ctfTestPulses[1001]) },
		"another signature": func(string) (int, string) { return http.StatusOK, ctfTestRelayBody(1000, ctfTestPulses[1001]) },
		"wrong randomness": func(string) (int, string) {
			return http.StatusOK, `{"round":1000,"randomness":"00","signature":"` + ctfTestPulses[1000] + `"}`
		},
		"not hex": func(string) (int, string) { return http.StatusOK, `{"round":1000,"signature":"zz"}` },
		"short": func(string) (int, string) {
			return http.StatusOK, `{"round":1000,"signature":"` + ctfTestPulses[1000][:94] + `"}`
		},
		"too early":           func(string) (int, string) { return 425, "" },
		"not json":            func(string) (int, string) { return http.StatusOK, "<html>" },
		"oversized":           func(string) (int, string) { return http.StatusOK, strings.Repeat(" ", stCtfDrandMaxBytes+1) },
		"unavailable":         func(string) (int, string) { return http.StatusServiceUnavailable, "" },
		"flipped signature":   func(string) (int, string) { return http.StatusOK, ctfTestRelayBody(1000, "a"+ctfTestPulses[1000][1:]) },
		"another chain's sig": func(string) (int, string) { return http.StatusOK, ctfTestRelayBody(1000, strings.Repeat("00", 48)) },
	} {
		t.Run(name, func(t *testing.T) {
			resetCtfPulses(t)
			bad, badAsked := ctfTestRelay(t, answer)
			if _, err := stCtfDrandPulse(t.Context(), []string{bad.URL}, 1000); err == nil {
				t.Fatal("a pulse that does not verify was taken")
			}
			*goodAsked = nil
			pulse, err := stCtfDrandPulse(t.Context(), []string{bad.URL, good.URL + "/"}, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if pulse.Round != 1000 || hex.EncodeToString(pulse.Signature) != ctfTestPulses[1000] || len(*badAsked) != 2 || len(*goodAsked) != 1 {
				t.Fatalf("pulse %d %x, asked %v then %v", pulse.Round, pulse.Signature, *badAsked, *goodAsked)
			}
		})
	}

	// kept: no relay is asked again
	resetCtfPulses(t)
	if _, err := stCtfDrandPulse(t.Context(), []string{good.URL}, 1000); err != nil {
		t.Fatal(err)
	}
	*goodAsked = nil
	if pulse, err := stCtfDrandPulse(t.Context(), []string{"http://drand.invalid"}, 1000); err != nil || pulse.Round != 1000 || len(*goodAsked) != 0 {
		t.Fatalf("kept pulse: %v %v", pulse, err)
	}
}

// §4.4: a round whose time has not come is never asked for, and no relay
// answering is an error naming each relay's problem.
func TestStCtfDrandPulseWaitsForItsRound(t *testing.T) {
	resetCtfPulses(t)
	relay, asked := ctfTestRelay(t, func(round string) (int, string) { return 425, "" })
	now := stCtfServerNow
	t.Cleanup(func() { stCtfServerNow = now })
	stCtfServerNow = func() time.Time { return time.Unix(int64(ctf.RoundTime(5000))-1, 0).UTC() }
	if _, err := stCtfDrandPulse(t.Context(), []string{relay.URL}, 5000); !errors.Is(err, errStCtfPulseNotYet) || len(*asked) != 0 {
		t.Fatalf("round not published yet: %v, asked %v", err, *asked)
	}
	stCtfServerNow = func() time.Time { return time.Unix(int64(ctf.RoundTime(5000)), 0).UTC() }
	_, err := stCtfDrandPulse(t.Context(), []string{relay.URL, relay.URL}, 5000)
	var drandErr *stCtfDrandError
	if !errors.As(err, &drandErr) || drandErr.Round != 5000 || len(drandErr.Problems) != 2 || len(*asked) != 2 {
		t.Fatalf("no relay answered: %v, asked %v", err, *asked)
	}
	if _, err := stCtfDrandPulse(t.Context(), nil, 5000); err == nil {
		t.Fatal("a pulse with no relay")
	}
	if _, err := stCtfDrandPulse(t.Context(), []string{relay.URL}, 0); err == nil {
		t.Fatal("round 0")
	}
}

// §4.4: ρ_k is the first round at least 60 s after the census block, so a
// commitment at T is drawn with the round of T + 60, never an earlier one.
func TestStCtfDrawRoundIsSixtySecondsAfterTheCensusBlock(t *testing.T) {
	for _, round := range []uint64{1000, 1001, 1004} {
		at := ctfTestCommitTime(round)
		if got := ctf.DrawRound(at); got != round {
			t.Fatalf("commit at %d: round %d, want %d", at, got, round)
		}
		if stCtfRoundTime(round).Unix() < int64(at)+ctf.DrawDelay {
			t.Fatalf("round %d at %s is less than 60 s after %d", round, stCtfRoundTime(round), at)
		}
		if got := ctf.DrawRound(at + 1); got != round+1 {
			t.Fatalf("commit at %d: round %d, want %d", at+1, got, round+1)
		}
	}
}
