package controller

// A failed Helius lookup is answered in the result's body with its stable
// code. The transport error, whose url carries the Helius api key, was
// returned beside the result, so the router dropped the result and sent the
// error's text with a 500.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A transport error quoting a synthetic api key in its url, as Helius's does.
var seekerLookupTestFailure = errors.New(`Post "https://mainnet.helius-rpc.com/?api-key=synthetic-helius-key": dial tcp 192.0.2.9:443: connect: connection refused`)

// A lookup whose asset search fails, or whose saga search fails.
func seekerLookupTestFailing(saga bool) seekerHolderLookup {
	return seekerHolderLookup{
		searchAssets: func(context.Context, string) ([]HeliusAsset, error) {
			if saga {
				return nil, nil
			}
			return nil, seekerLookupTestFailure
		},
		searchSagaAssets: func(context.Context, string) ([]HeliusAsset, error) {
			return nil, seekerLookupTestFailure
		},
		markHolder: func(string, *session.ClientSession) error { return errors.New("unexpected holder mark") },
	}
}

func TestVerifySeekerNftHolderLookupFailureAnswersItsCode(t *testing.T) {
	for _, saga := range []bool{false, true} {
		test := newSeekerVerifyTest(t)
		result, err := verifySeekerNftHolder(test.verify, session.Testing_CreateClientSession(context.Background(), nil), seekerLookupTestFailing(saga))
		if err != nil || result == nil || result.Success || result.Error == nil || result.Error.Code != VerifySeekerNftHolderErrorCodeLookupFailed {
			t.Fatalf("saga=%t lookup failure was not answered with its code: %+v %v", saga, result, err)
		}
	}
}

// The same through the router: the body carries the code, never the key.
func TestVerifySeekerNftHolderLookupFailureSendsNoApiKey(t *testing.T) {
	test := newSeekerVerifyTest(t)
	encoded, err := json.Marshal(test.verify)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/account/wallets/verify-seeker", bytes.NewReader(encoded))
	request.RemoteAddr = "192.0.2.52:4500"
	response := httptest.NewRecorder()
	router.WrapWithInputNoAuth(func(verify *VerifySeekerNftHolderArgs, clientSession *session.ClientSession) (*VerifySeekerNftHolderResult, error) {
		return verifySeekerNftHolder(verify, clientSession, seekerLookupTestFailing(false))
	}, response, request)
	if strings.Contains(response.Body.String(), "synthetic-helius-key") || response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":"seeker_lookup_failed"`) {
		t.Fatalf("lookup failure sent its transport error: %d %q", response.Code, response.Body.String())
	}
}

// A logged Helius error never carries the api key.
func TestHeliusErrorRedactsTheApiKey(t *testing.T) {
	err := heliusError("synthetic-helius-key", seekerLookupTestFailure)
	if err == nil || strings.Contains(err.Error(), "synthetic-helius-key") || !strings.Contains(err.Error(), "api-key=[redacted]") {
		t.Fatalf("helius error kept its key: %v", err)
	}
	if heliusError("synthetic-helius-key", nil) != nil {
		t.Fatal("no error became an error")
	}
}
