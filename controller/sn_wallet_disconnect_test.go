package controller

// The payout wallet disconnect's gate and refusals, without a database: only
// the network owner's own sign-in passes, and every model refusal reads as its
// coded answer.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Every model refusal has its code, a rate limit carries its wait, and any
// other error stays an error.
func TestSnWalletDisconnectModelRefusals(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{model.ErrStWalletDisconnectOwnerOnly, SnWalletDisconnectErrorCodeOwnerOnly},
		{model.ErrStWalletDisconnectNoWallet, SnWalletDisconnectErrorCodeNoWallet},
		{model.ErrStWalletDisconnectEmailRequired, SnWalletDisconnectErrorCodeEmailRequired},
		{model.ErrStWalletDisconnectCodeInvalid, SnWalletDisconnectErrorCodeCodeInvalid},
		{model.ErrStWalletDisconnectCodeExpired, SnWalletDisconnectErrorCodeCodeExpired},
	} {
		refusal, err := snWalletDisconnectModelRefusal(fmt.Errorf("wrapped: %w", c.err))
		if err != nil || refusal == nil || refusal.Code != c.code || refusal.Message == "" {
			t.Errorf("%v: refusal = %+v, err = %v, want %s", c.err, refusal, err, c.code)
		}
	}

	rateLimit := &verifySendStatusError{message: "429 Too many codes.", retryAfterSeconds: 600}
	refusal, err := snWalletDisconnectModelRefusal(rateLimit)
	if err != nil || refusal.Code != SnWalletDisconnectErrorCodeRateLimited || refusal.RetryAfterSeconds != 600 || strings.HasPrefix(refusal.Message, "429") {
		t.Fatalf("rate limit: refusal = %+v, err = %v", refusal, err)
	}

	other := errors.New("database unavailable")
	if refusal, err := snWalletDisconnectModelRefusal(other); refusal != nil || !errors.Is(err, other) {
		t.Fatalf("another error: refusal = %+v, err = %v", refusal, err)
	}
}

// A session that is not the owner's is refused before the consent or the
// model is read: the gate needs no deployment and no database.
func TestSnWalletDisconnectRefusesBeforeReading(t *testing.T) {
	clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()})
	clientSession.ApiKeyAuthenticated = true

	sent, err := snWalletDisconnectCode(clientSession, model.DefaultStWalletDisconnectSettings(), nil)
	if err != nil || sent.Error == nil || sent.Error.Code != SnWalletDisconnectErrorCodeOwnerOnly || len(sent.SentTo) != 0 {
		t.Fatalf("code: result = %+v, err = %v", sent, err)
	}
	disconnected, err := snWalletDisconnect(&SnWalletDisconnectArgs{Code: "123456"}, clientSession, model.DefaultStWalletDisconnectSettings())
	if err != nil || disconnected.Error == nil || disconnected.Error.Code != SnWalletDisconnectErrorCodeOwnerOnly || disconnected.Disconnected {
		t.Fatalf("disconnect: result = %+v, err = %v", disconnected, err)
	}
}
