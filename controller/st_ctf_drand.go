package controller

// st_ctf_drand.go — the drand quicknet pulse that seeds an epoch's draw (sn
// CAPTURETHEFLAG.md §4.4). ρ_k is the first quicknet round whose time is at
// least 60 s after the census commitment's block, so the candidates are fixed
// on chain before the randomness exists. The pulse is read from drand's public
// HTTP relays and kept only once its BLS signature verifies against quicknet's
// public key (ctf.Pulse.Verify, sn/crv4's constants): a relay is trusted for
// nothing, and every relay serves the same pulse, since quicknet's unchained
// signature of a round is unique. Verified pulses are kept for the process's
// life; a round that is not published yet is never asked for.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urfoundation/sn/crv4"
	"github.com/urfoundation/sn/ctf"
)

const (
	// stCtfDrandTimeout bounds one relay's answer.
	stCtfDrandTimeout = 10 * time.Second
	// stCtfDrandMaxBytes bounds a relay's body: a pulse is about 250 bytes.
	stCtfDrandMaxBytes = 4096
	// stCtfDrandAlertAfter is how long after its round's time a pulse may be
	// unavailable before StCtfAlertDrand is raised.
	stCtfDrandAlertAfter = 10 * time.Minute
)

// errStCtfPulseNotYet is a round whose time has not come.
var errStCtfPulseNotYet = errors.New("st ctf: the drand round is not published yet")

// stCtfDrandHttp is the relays' client; tests replace it.
var stCtfDrandHttp = &http.Client{
	Timeout: stCtfDrandTimeout,
	// A redirect would leave the relay named in configuration.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

var stCtfPulses sync.Map // round → *ctf.Pulse, verified

// stCtfRoundTime is the time quicknet publishes round r.
func stCtfRoundTime(round uint64) time.Time {
	return time.Unix(int64(ctf.RoundTime(round)), 0).UTC()
}

// stCtfDrandPulse returns quicknet's verified pulse of a round, from the
// first relay that serves one that verifies. A relay that answers with
// anything else is skipped and named in the error.
func stCtfDrandPulse(ctx context.Context, urls []string, round uint64) (*ctf.Pulse, error) {
	if round == 0 {
		return nil, errors.New("st ctf: drand round 0 does not exist")
	}
	if value, ok := stCtfPulses.Load(round); ok {
		return value.(*ctf.Pulse), nil
	}
	if at := stCtfRoundTime(round); stCtfServerNow().Before(at) {
		return nil, fmt.Errorf("%w: round %d at %s", errStCtfPulseNotYet, round, at.Format(time.RFC3339))
	}
	if len(urls) == 0 {
		return nil, errors.New("st ctf: no drand relay is configured")
	}
	problems := []string{}
	for _, base := range urls {
		pulse, err := stCtfDrandFetch(ctx, base, round)
		if err == nil {
			stCtfPulses.Store(round, pulse)
			stCtfDrandFetches.WithLabelValues("verified").Inc()
			return pulse, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		result := "unavailable"
		if errors.Is(err, ctf.ErrPulse) {
			result = "refused"
		}
		stCtfDrandFetches.WithLabelValues(result).Inc()
		problems = append(problems, fmt.Sprintf("%s: %s", base, err))
	}
	return nil, &stCtfDrandError{Round: round, Problems: problems}
}

// stCtfDrandError is a round no relay served a verified pulse of.
type stCtfDrandError struct {
	Round    uint64
	Problems []string
}

func (self *stCtfDrandError) Error() string {
	return fmt.Sprintf("st ctf: no relay served a verified pulse of round %d: %s", self.Round, strings.Join(self.Problems, "; "))
}

// stCtfDrandAlert alerts a pulse still unavailable stCtfDrandAlertAfter after
// its round's time.
func stCtfDrandAlert(cfg *StConfig, err error) {
	var drandErr *stCtfDrandError
	if errors.As(err, &drandErr) && stCtfRoundTime(drandErr.Round).Add(stCtfDrandAlertAfter).Before(stCtfServerNow()) {
		stCtfAlert(cfg, StCtfAlertDrand, fmt.Sprintf("round %d", drandErr.Round), "%s", drandErr)
	}
}

// stCtfDrandAnswer is a relay's answer for one quicknet round.
type stCtfDrandAnswer struct {
	Round      uint64 `json:"round"`
	Signature  string `json:"signature"`
	Randomness string `json:"randomness"`
}

// stCtfDrandFetch reads one round from one relay and verifies it.
func stCtfDrandFetch(ctx context.Context, base string, round uint64) (*ctf.Pulse, error) {
	ctx, cancel := context.WithTimeout(ctx, stCtfDrandTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/%s/public/%d", strings.TrimSuffix(base, "/"), crv4.QuicknetChainHash, round), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := stCtfDrandHttp.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, stCtfDrandMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > stCtfDrandMaxBytes {
		return nil, fmt.Errorf("more than %d bytes", stCtfDrandMaxBytes)
	}
	var answer stCtfDrandAnswer
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, err
	}
	if answer.Round != round {
		return nil, fmt.Errorf("%w: asked for round %d, served round %d", ctf.ErrPulse, round, answer.Round)
	}
	signature, err := hex.DecodeString(strings.TrimPrefix(answer.Signature, "0x"))
	if err != nil {
		return nil, fmt.Errorf("%w: round %d: signature is not hex", ctf.ErrPulse, round)
	}
	pulse := &ctf.Pulse{Round: round, Signature: signature}
	if err := pulse.Verify(); err != nil {
		return nil, err
	}
	// The randomness a relay states is only a convenience; when it is stated
	// it must be the signature's sha256, else the relay is not serving
	// quicknet as drand defines it.
	if answer.Randomness != "" {
		randomness := sha256.Sum256(signature)
		if !strings.EqualFold(strings.TrimPrefix(answer.Randomness, "0x"), hex.EncodeToString(randomness[:])) {
			return nil, fmt.Errorf("%w: round %d: randomness is not sha256 of the signature", ctf.ErrPulse, round)
		}
	}
	return pulse, nil
}
