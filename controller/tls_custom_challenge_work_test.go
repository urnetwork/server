package controller

// The challenge handoff's pieces that need no database (QD-1165): what the install refuses, the value a POST takes,
// how a refused receipt answers, and a run in a process with nothing installed. The receipt's steps and the routes
// over Postgres, the dns block and the queue are model's and api's sidecar_pending tests.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Only deps the command can run on install: a store, a DNS client, and settings that validate; an uninstall
// removes only what it installed.
func TestTlsCustomChallengeWorkerInstallRefusesWhatCannotRun(t *testing.T) {
	store, err := model.NewTlsCustomStore(&model.TlsCustomStoreOptions{Roots: []string{"root.example"}})
	if err != nil {
		t.Fatal(err)
	}
	defer SetTlsCustomChallengeWorker(nil)
	stalled := model.DefaultTlsCustomChallengeSettings()
	stalled.PollInterval = 0
	for _, deps := range []*TlsCustomChallengeWorkerDeps{
		nil,
		{Dns: certsservice.ClosedCertsDns{}},
		{Store: store},
		{Store: store, Dns: certsservice.ClosedCertsDns{}, Settings: stalled},
	} {
		if installed := SetTlsCustomChallengeWorker(deps); installed != nil || tlsCustomChallengeWorkerDeps.Load() != nil {
			t.Fatalf("%+v installed", deps)
		}
	}
	installed := SetTlsCustomChallengeWorker(&TlsCustomChallengeWorkerDeps{Store: store, Dns: certsservice.ClosedCertsDns{}})
	if installed == nil || tlsCustomChallengeWorkerDeps.Load() != installed {
		t.Fatal("complete deps were not installed")
	}
	UnsetTlsCustomChallengeWorker(&TlsCustomChallengeWorkerDeps{Store: store, Dns: certsservice.ClosedCertsDns{}})
	if tlsCustomChallengeWorkerDeps.Load() != installed {
		t.Fatal("another install's uninstall removed these deps")
	}
	UnsetTlsCustomChallengeWorker(installed)
	if tlsCustomChallengeWorkerDeps.Load() != nil {
		t.Fatal("the uninstall left its deps")
	}

	// the routes' install refuses settings that do not validate in the same way
	defer SetTlsCustom(nil)
	SetTlsCustom(&TlsCustomDeps{
		Store:      store,
		Caller:     func(context.Context, *sidecar.Principal, time.Time) (*model.TlsCustomCaller, error) { return nil, nil },
		Roots:      []string{"root.example"},
		Challenges: stalled,
	})
	if tlsCustomDeps.Load() != nil {
		t.Fatal("the routes installed settings that do not validate")
	}
}

// A POST's value is the unpadded base64url of a SHA-256 digest, nothing else.
func TestTlsCustomChallengeValueIsADigest(t *testing.T) {
	digest := sha256.Sum256([]byte("token.thumbprint"))
	value := base64.RawURLEncoding.EncodeToString(digest[:])
	cases := []struct {
		value string
		valid bool
	}{
		{value: value, valid: true},
		{value: value[:42], valid: false},
		{value: value + "A", valid: false},
		{value: strings.Repeat("A", 42) + "*", valid: false},
		{value: base64.URLEncoding.EncodeToString(digest[:]), valid: false},
		{value: "", valid: false},
	}
	for _, c := range cases {
		if valid := tlsCustomChallengeValueValid(c.value); valid != c.valid {
			t.Errorf("%q: %t, want %t", c.value, valid, c.valid)
		}
	}
}

// A refused receipt answers as the custom routes answer: 409 claim, 404 no_account, 429 budget with the window's
// wait, and 503 for a write the DNS path refused otherwise or never made.
func TestTlsCustomChallengeRefusalAnswers(t *testing.T) {
	cases := []struct {
		refusal      string
		status       int
		reason       string
		retryAfterMs uint32
	}{
		{refusal: model.TlsCustomChallengeRefusalClaim, status: http.StatusConflict, reason: sidecar.TlsCustomRefusalClaim},
		{refusal: model.TlsCustomChallengeRefusalNoAccount, status: http.StatusNotFound, reason: sidecar.TlsCustomRefusalNoAccount},
		{refusal: model.TlsCustomChallengeRefusalBudget, status: http.StatusTooManyRequests, reason: sidecar.TlsCustomRefusalBudget, retryAfterMs: 90000},
	}
	for _, c := range cases {
		err := tlsCustomChallengeRefusal(&model.TlsCustomChallengeReceipt{ChallengeId: server.NewId(), Refusal: c.refusal, RetryAfter: time.Duration(c.retryAfterMs) * time.Millisecond})
		var refusal *sidecar.TlsCustomError
		if !errors.As(err, &refusal) || refusal.Status != c.status || refusal.Refusal.Reason != c.reason || refusal.Refusal.RetryAfterMs != c.retryAfterMs {
			t.Errorf("%s answered %v", c.refusal, err)
		}
	}
	for _, refusal := range []string{model.TlsCustomChallengeRefusalRefused, model.TlsCustomChallengeRefusalAttempts} {
		if err := tlsCustomChallengeRefusal(&model.TlsCustomChallengeReceipt{ChallengeId: server.NewId(), Refusal: refusal}); !errors.Is(err, errTlsCertsUnavailable) {
			t.Errorf("%s answered %v", refusal, err)
		}
	}
}

// A process with nothing installed leaves the receipt as it is and queues no run: the next capture or ask does.
func TestTlsCustomChallengeWorkWithNothingInstalled(t *testing.T) {
	SetTlsCustomChallengeWorker(nil)
	clientSession := session.NewLocalClientSession(context.Background(), "", nil)
	defer clientSession.Cancel()
	result, err := TlsCustomChallengeWork(&TlsCustomChallengeWorkArgs{ChallengeId: server.NewId()}, clientSession)
	if err != nil || result == nil || !result.RunAt.IsZero() || result.WriteCount != 0 || result.ReadyCount != 0 {
		t.Fatalf("%+v, %v", result, err)
	}
	// no run owed: the post queues nothing, so it needs no transaction
	if err := TlsCustomChallengeWorkPost(&TlsCustomChallengeWorkArgs{ChallengeId: server.NewId()}, result, clientSession, nil); err != nil {
		t.Fatal(err)
	}
}
