// Public customer challenge controls use actual request/journal paths and
// instance-owned synthetic HTTP responses. No live token or transfer is used.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

type customerTransferFixture struct {
	owner       *session.ClientSession
	args        *WalletCircleTransferOutArgs
	client      *circleTransferClient
	challenge   string
	tokens      atomic.Int64
	walletReads atomic.Int64
}

func customerTransferResponse(status int, value any) *http.Response {
	body, err := json.Marshal(value)
	server.Raise(err)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}
}

func newCustomerTransferFixture(t testing.TB, action func(*http.Request) (*http.Response, error)) *customerTransferFixture {
	t.Helper()
	user := server.NewId()
	wallet := server.NewId().String()
	token := server.NewId().String()
	requestId := server.NewId()
	f := &customerTransferFixture{challenge: server.NewId().String(), args: &WalletCircleTransferOutArgs{RequestId: &requestId, Terms: true, ToAddress: "synthetic-destination", AmountUsdcNanoCents: 1_000_001_000}}
	f.client = &circleTransferClient{defaultName: "Synthetic native", defaultChain: "SYNTHETIC", core: CoreCircleApiClient{readToken: func() string { return "synthetic-api-token" }}}
	f.client.tokenSource = func(owner *session.ClientSession) (*CircleUserToken, error) {
		f.tokens.Add(1)
		if err := owner.Ctx.Err(); err != nil {
			return nil, err
		}
		return &CircleUserToken{circleUserId: user, UserToken: "synthetic-user-token", EncryptionKey: "synthetic-encryption-key"}, nil
	}
	f.client.core.readHooks.do = func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/w3s/wallets" {
			f.walletReads.Add(1)
			return customerTransferResponse(200, map[string]any{"data": map[string]any{"wallets": []any{map[string]any{"id": wallet, "userId": user.String(), "address": "synthetic-source", "blockchain": "SYNTHETIC", "createDate": "2026-10-01T00:00:00Z"}}}}), nil
		}
		if request.Method == http.MethodGet && request.URL.Path == "/v1/w3s/wallets/"+wallet+"/balances" {
			return customerTransferResponse(200, map[string]any{"data": map[string]any{"tokenBalances": []any{map[string]any{"amount": "100.000001", "token": map[string]any{"id": token, "name": "Synthetic stablecoin", "symbol": "USDC", "blockchain": "SYNTHETIC", "isNative": false}}}}}), nil
		}
		return action(request)
	}
	f.owner = &session.ClientSession{Ctx: context.WithValue(t.Context(), circleTransferClientKey{}, f.client), ByJwt: &jwt.ByJwt{NetworkId: server.NewId(), UserId: server.NewId()}}
	return f
}

func (self *customerTransferFixture) retained(t testing.TB) *model.CircleTransferRequest {
	t.Helper()
	value, err := model.GetCircleTransferRequest(t.Context(), self.owner.ByJwt.NetworkId, self.owner.ByJwt.UserId, *self.args.RequestId)
	if err != nil || value == nil {
		t.Fatal("missing durable customer intent", err)
	}
	return value
}

func TestCustomerChallengeMissingIdAndInexactAmountRefuseBeforeEffects(t *testing.T) {
	var effects atomic.Int64
	f := newCustomerTransferFixture(t, func(*http.Request) (*http.Response, error) {
		effects.Add(1)
		return nil, fmt.Errorf("unexpected external call")
	})
	for _, args := range []*WalletCircleTransferOutArgs{nil, {Terms: true, ToAddress: "synthetic-destination", AmountUsdcNanoCents: 1000}} {
		if _, err := WalletCircleTransferOut(args, f.owner); !errors.Is(err, ErrCircleTransferRequestId) {
			t.Fatal("older caller missing durable id was admitted", err)
		}
	}
	f.args.AmountUsdcNanoCents = 1001
	if _, err := WalletCircleTransferOut(f.args, f.owner); err == nil {
		t.Fatal("rounded monetary amount was admitted")
	}
	canceled, cancel := context.WithCancel(f.owner.Ctx)
	cancel()
	f.owner.Ctx = canceled
	if _, err := WalletCircleTransferOut(f.args, f.owner); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation cause lost", err)
	}
	if f.tokens.Load() != 0 || f.walletReads.Load() != 0 || effects.Load() != 0 {
		t.Fatal("refused request performed external work")
	}
}

func TestCustomerChallengeLostResponseReusesExactDurableKeyAndBody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		var bodies []string
		var reads int
		var f *customerTransferFixture
		f = newCustomerTransferFixture(t, func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodPost {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				bodies = append(bodies, string(body))
				if len(bodies) == 1 {
					return nil, io.ErrUnexpectedEOF
				}
				return customerTransferResponse(201, map[string]any{"data": map[string]any{"challengeId": f.challenge}}), nil
			}
			reads++
			return customerTransferResponse(200, map[string]any{"data": map[string]any{"challenge": map[string]any{"id": f.challenge, "type": "CREATE_TRANSACTION", "status": "COMPLETE"}}}), nil
		})
		if _, err := WalletCircleTransferOut(f.args, f.owner); !errors.Is(err, ErrCircleTransferUnknown) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal("accepted-but-unobserved response became success", err)
		}
		original := f.retained(t)
		if original.ChallengeId != nil || original.SubmissionCount != 1 {
			t.Fatal("unknown response invented an accepted challenge")
		}
		// A separate request owner models API retry/restart; it must not rerun
		// wallet selection or replace the persisted body with current defaults.
		restartedClient := *f.client
		restartedClient.defaultChain = "CHANGED"
		restartedOwner := *f.owner
		restartedOwner.Ctx = context.WithValue(t.Context(), circleTransferClientKey{}, &restartedClient)
		result, err := WalletCircleTransferOut(f.args, &restartedOwner)
		if err != nil || result == nil || result.ChallengeId != f.challenge || len(bodies) != 2 || bodies[0] != bodies[1] || bodies[0] != original.Body {
			t.Fatal("retry changed logical transfer", err, bodies)
		}
		result, err = WalletCircleTransferOut(f.args, &restartedOwner)
		if err != nil || result == nil || result.ChallengeStatus != "COMPLETE" || len(bodies) != 2 || reads != 1 || f.walletReads.Load() != 1 {
			t.Fatal("known challenge was resubmitted instead of observed", err)
		}
		retained := f.retained(t)
		if retained.IdempotencyKey != original.IdempotencyKey || retained.SubmissionCount != 2 {
			t.Fatal("reconciliation erased attempt attribution")
		}
		var body struct {
			Amounts []string `json:"amounts"`
			RefId   string   `json:"refId"`
		}
		if json.Unmarshal([]byte(original.Body), &body) != nil || len(body.Amounts) != 1 || body.Amounts[0] != "1.000001" || body.RefId != f.args.RequestId.String() {
			t.Fatal("wire amount or caller reference changed")
		}
	})
}

func TestCustomerChallengeConcurrentRetriesCannotMintDistinctChallenges(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		entered := make(chan struct{})
		var calls atomic.Int64
		var stateLock sync.Mutex
		requests := map[string]bool{}
		var f *customerTransferFixture
		f = newCustomerTransferFixture(t, func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			func() { stateLock.Lock(); defer stateLock.Unlock(); requests[string(body)] = true }()
			if calls.Add(1) == 2 {
				close(entered)
			}
			select {
			case <-entered:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			return customerTransferResponse(201, map[string]any{"data": map[string]any{"challengeId": f.challenge}}), nil
		})
		start := make(chan struct{})
		done := make(chan error, 2)
		for range 2 {
			go func() {
				<-start
				result, err := WalletCircleTransferOut(f.args, f.owner)
				if err == nil && (result == nil || result.ChallengeId != f.challenge) {
					err = fmt.Errorf("wrong challenge")
				}
				done <- err
			}()
		}
		close(start)
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		if calls.Load() != 2 || len(requests) != 1 {
			t.Fatal("simultaneous calls escaped one provider idempotency basis", calls.Load(), len(requests))
		}
		if value := f.retained(t); value.ChallengeId == nil || value.SubmissionCount != 2 {
			t.Fatal("concurrent result not retained")
		}
	})
}

func TestCustomerChallengeCancellationAfterRealResponseRetainsOriginalRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		var cancel context.CancelFunc
		var calls int
		var f *customerTransferFixture
		f = newCustomerTransferFixture(t, func(request *http.Request) (*http.Response, error) {
			calls++
			cancel()
			return customerTransferResponse(201, map[string]any{"data": map[string]any{"challengeId": f.challenge}}), nil
		})
		f.owner.Ctx, cancel = context.WithCancel(f.owner.Ctx)
		defer cancel()
		if _, err := WalletCircleTransferOut(f.args, f.owner); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled response was acknowledged without typed cause", err)
		}
		retained := f.retained(t)
		if calls != 1 || retained.SubmissionCount != 1 || retained.ChallengeId != nil {
			t.Fatal("cancellation lost or invented remote outcome")
		}
	})
}

func TestCustomerChallengeKnownObservationFailureNeverFallsBackToPost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		var posts, reads int
		var f *customerTransferFixture
		f = newCustomerTransferFixture(t, func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodPost {
				posts++
				return customerTransferResponse(201, map[string]any{"data": map[string]any{"challengeId": f.challenge}}), nil
			}
			reads++
			return customerTransferResponse(404, map[string]any{"message": "synthetic missing challenge"}), nil
		})
		if _, err := WalletCircleTransferOut(f.args, f.owner); err != nil {
			t.Fatal(err)
		}
		original := f.retained(t)
		if _, err := WalletCircleTransferOut(f.args, f.owner); err == nil {
			t.Fatal("missing known challenge was accepted")
		}
		f.args.ToAddress = "different-destination"
		if _, err := WalletCircleTransferOut(f.args, f.owner); !errors.Is(err, model.ErrCircleTransferRequestChanged) {
			t.Fatal("original intent was overwritten", err)
		}
		if posts != 1 || reads != 1 || f.retained(t).Body != original.Body {
			t.Fatal("known challenge read failure minted another write")
		}
	})
}

func TestCustomerChallengeMalformedAcceptedResponseRemainsUnknown(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		var posts int
		f := newCustomerTransferFixture(t, func(*http.Request) (*http.Response, error) {
			posts++
			return customerTransferResponse(201, map[string]any{"data": map[string]any{"challengeId": "not-an-identity"}}), nil
		})
		if _, err := WalletCircleTransferOut(f.args, f.owner); !errors.Is(err, ErrCircleTransferUnknown) {
			t.Fatal("malformed write response reported success", err)
		}
		if posts != 1 || f.retained(t).ChallengeId != nil {
			t.Fatal("malformed response discarded request or invented challenge")
		}
	})
}

func TestCustomerChallengeRepeatedObservationPreservesTerminalCapacity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		var posts, reads int
		var f *customerTransferFixture
		f = newCustomerTransferFixture(t, func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodPost {
				posts++
				return customerTransferResponse(201, map[string]any{"data": map[string]any{"challengeId": f.challenge}}), nil
			}
			reads++
			status := "IN_PROGRESS"
			if reads == 67 {
				status = "COMPLETE"
			}
			return customerTransferResponse(200, map[string]any{"data": map[string]any{"challenge": map[string]any{"id": f.challenge, "type": "CREATE_TRANSACTION", "status": status, "syntheticMetadata": reads}}}), nil
		})
		if _, err := WalletCircleTransferOut(f.args, f.owner); err != nil {
			t.Fatal(err)
		}
		for range 67 {
			if _, err := WalletCircleTransferOut(f.args, f.owner); err != nil {
				t.Fatal("healthy repeated observation exhausted terminal custody capacity", reads, err)
			}
		}
		if value := f.retained(t); value.ChallengeStatus != "COMPLETE" || posts != 1 || reads != 67 {
			t.Fatal("repeated observations lost final challenge or caused new write")
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(t.Context(), `SELECT count(*) FROM circle_transfer_observation`).Scan(&count))
			if count != 3 {
				t.Fatal("raw response variation consumed semantic history slots", count)
			}
		})
	})
}

func TestCustomerChallengeKnownReadCannotMutateWithoutCustodyGuards(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		var posts, reads int
		var f *customerTransferFixture
		f = newCustomerTransferFixture(t, func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodPost {
				posts++
				return customerTransferResponse(201, map[string]any{"data": map[string]any{"challengeId": f.challenge}}), nil
			}
			reads++
			return customerTransferResponse(200, map[string]any{"data": map[string]any{"challenge": map[string]any{"id": f.challenge, "type": "CREATE_TRANSACTION", "status": "COMPLETE"}}}), nil
		})
		if _, err := WalletCircleTransferOut(f.args, f.owner); err != nil {
			t.Fatal(err)
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(t.Context(), `ALTER TABLE circle_transfer_observation DISABLE TRIGGER circle_transfer_observation_guard`))
		})
		if _, err := WalletCircleTransferOut(f.args, f.owner); err == nil {
			t.Fatal("known read wrote new evidence with a disabled custody guard")
		}
		if value := f.retained(t); value.ChallengeStatus != "PENDING" || posts != 1 || reads != 1 {
			t.Fatal("guard refusal replaced evidence or blocked the read itself")
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(t.Context(), `ALTER TABLE circle_transfer_observation ENABLE TRIGGER circle_transfer_observation_guard`))
		})
		if value, err := WalletCircleTransferOut(f.args, f.owner); err != nil || value == nil || value.ChallengeStatus != "COMPLETE" || posts != 1 {
			t.Fatal("restored custody could not retain the original challenge observation", err)
		}
	})
}
