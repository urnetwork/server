// These controls use real request/decoder paths with owned synthetic transports.
// Only retry time is accelerated; response bodies and cancellation still join.
package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func circleReadTestJson(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func circleReadTestWallet(id string) map[string]any {
	return map[string]any{"id": id, "address": "synthetic-address-" + id, "blockchain": "SYNTHETIC", "createDate": "2026-10-01T00:00:00Z"}
}

func circleReadTestBalance(id, amount string, native bool) map[string]any {
	symbol := "USDC"
	if native {
		symbol = "SYN"
	}
	return map[string]any{"amount": amount, "token": map[string]any{"id": id, "blockchain": "SYNTHETIC", "isNative": native, "name": "Synthetic native", "symbol": symbol}}
}

func circleReadTestPage(t *testing.T, key string, items any) string {
	t.Helper()
	return circleReadTestJson(t, map[string]any{"data": map[string]any{key: items}})
}

func circleReadTestNext(request *http.Request, cursor string) string {
	uri := *request.URL
	query := uri.Query()
	query.Set("pageAfter", cursor)
	uri.RawQuery = query.Encode()
	return "<" + uri.String() + ">; rel=\"next\""
}

func circleReadTestList(t *testing.T, client *CoreCircleApiClient, ctx context.Context, tokens *int) ([]*CircleWalletInfo, error) {
	t.Helper()
	return client.findCircleWallets(&session.ClientSession{Ctx: ctx}, func(owner *session.ClientSession) (*CircleUserToken, error) {
		*tokens++
		if owner.Ctx.Err() != nil {
			return nil, owner.Ctx.Err()
		}
		if _, ok := owner.Ctx.Deadline(); !ok {
			t.Fatal("token creation did not borrow the logical read deadline")
		}
		return &CircleUserToken{circleUserId: server.NewId(), UserToken: "synthetic-user-token"}, nil
	}, "Synthetic native", "SYNTHETIC")
}

func TestCircleReadEntityKeyRetriesAndRejectsMalformedKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	public := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	for _, malformed := range []bool{false, true} {
		calls, closed, waits := 0, 0, 0
		now := time.Now()
		client := paymentReadTestCircle(paymentReadHooks{
			now: func() time.Time { return now },
			wait: func(ctx context.Context, delay time.Duration) error {
				waits++
				now = now.Add(delay)
				return ctx.Err()
			},
			do: func(request *http.Request) (*http.Response, error) {
				requirePaymentReadRequest(t, request)
				if !strings.HasSuffix(request.URL.Path, "/config/entity/publicKey") || calls != closed {
					t.Fatal("wrong entity read or unjoined prior body")
				}
				calls++
				if calls == 1 && !malformed {
					return paymentReadTestResponse(503, "temporary", &closed), nil
				}
				value := public
				if malformed {
					value = "synthetic-invalid-key"
				}
				return paymentReadTestResponse(200, circleReadTestPage(t, "publicKey", value), &closed), nil
			},
		})
		result, err := client.getPublicKey(context.Background())
		if malformed {
			if result != nil || err == nil || calls != 1 || closed != 1 || waits != 0 {
				t.Fatal("malformed entity key was accepted or retried", err, calls, closed, waits)
			}
		} else if result == nil || *result != public || err != nil || calls != 2 || closed != 2 || waits != 1 {
			t.Fatal("entity key did not survive transient read", err, calls, closed, waits)
		}
	}
}

func TestCircleReadNotificationBindsKeyAndSignature(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"synthetic":true}`)
	digest := sha256.Sum256(body)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	for _, defect := range []string{"", "id", "algorithm", "key", "signature"} {
		calls, closed, waits := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(ctx context.Context, delay time.Duration) error { waits++; return ctx.Err() },
			do: func(request *http.Request) (*http.Response, error) {
				requirePaymentReadRequest(t, request)
				if !strings.HasSuffix(request.URL.Path, "/notifications/publicKey/synthetic-key") {
					t.Fatal("notification key request identity changed")
				}
				calls++
				if calls == 1 && defect == "" {
					return paymentReadTestResponse(503, "temporary", &closed), nil
				}
				data := map[string]any{"id": "synthetic-key", "algorithm": "ECDSA_SHA_256", "publicKey": base64.StdEncoding.EncodeToString(der)}
				if defect == "id" {
					data["id"] = "different-key"
				} else if defect == "algorithm" {
					data["algorithm"] = "synthetic-unsupported"
				} else if defect == "key" {
					data["publicKey"] = "synthetic-invalid"
				}
				return paymentReadTestResponse(200, circleReadTestJson(t, map[string]any{"data": data}), &closed), nil
			},
		})
		supplied := body
		if defect == "signature" {
			supplied = []byte(`{"synthetic":false}`)
		}
		err := client.verifyCircleAuth(context.Background(), "synthetic-key", base64.StdEncoding.EncodeToString(signature), supplied)
		if defect == "" {
			if err != nil || calls != 2 || closed != 2 || waits != 1 {
				t.Fatal("valid signed notification did not recover", err, calls, closed, waits)
			}
		} else if err == nil || calls != 1 || closed != 1 || waits != 0 {
			t.Fatal("notification identity/signature mismatch retried or passed", defect, err, calls, closed, waits)
		}
	}
}

func TestCircleReadWalletIdentityAndRequiredFields(t *testing.T) {
	for _, defect := range []string{"", "id", "address", "blockchain", "createDate"} {
		calls, closed, waits := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(ctx context.Context, delay time.Duration) error { waits++; return ctx.Err() },
			do: func(request *http.Request) (*http.Response, error) {
				requirePaymentReadRequest(t, request)
				calls++
				if defect == "" && calls == 1 {
					return paymentReadTestResponse(500, "temporary", &closed), nil
				}
				wallet := circleReadTestWallet("synthetic-wallet")
				if defect == "id" {
					wallet[defect] = "different-wallet"
				} else if defect != "" {
					delete(wallet, defect)
				}
				return paymentReadTestResponse(200, circleReadTestPage(t, "wallet", wallet), &closed), nil
			},
		})
		wallet, err := client.getCircleWallet(context.Background(), "synthetic-wallet")
		if defect == "" {
			if err != nil || wallet == nil || wallet.ID != "synthetic-wallet" || calls != 2 || closed != 2 || waits != 1 {
				t.Fatal("wallet read did not recover", err, calls, closed, waits)
			}
		} else if err == nil || wallet != nil || calls != 1 || closed != 1 || waits != 0 {
			t.Fatal("invalid wallet response accepted/retried", defect, err, calls, closed, waits)
		}
	}
}

// Next cursors deliberately differ from both wallet ids and token ids. Final
// nonempty pages omit next; no speculative extra request is allowed.
func TestCircleReadWalletAndBalancePaginationUsesResponseLinks(t *testing.T) {
	calls, closed, tokens, waits := 0, 0, 0, 0
	secondBalanceAttempts := 0
	client := paymentReadTestCircle(paymentReadHooks{
		wait: func(ctx context.Context, delay time.Duration) error { waits++; return ctx.Err() },
		do: func(request *http.Request) (*http.Response, error) {
			requirePaymentReadRequest(t, request)
			if request.Header.Get("X-User-Token") != "synthetic-user-token" || request.Header.Get("Authorization") != "Bearer synthetic-read-token" || closed != calls {
				t.Fatal("credentials or prior body ownership changed")
			}
			calls++
			var payload, next string
			query := request.URL.Query()
			switch {
			case strings.HasSuffix(request.URL.Path, "/wallets"):
				switch query.Get("pageAfter") {
				case "":
					payload = circleReadTestPage(t, "wallets", []any{circleReadTestWallet("wallet-one")})
					next = circleReadTestNext(request, "opaque-wallet-next")
				case "opaque-wallet-next":
					payload = circleReadTestPage(t, "wallets", []any{circleReadTestWallet("wallet-two")})
				default:
					t.Fatal("wallet cursor was inferred from payload", query)
				}
			case strings.HasSuffix(request.URL.Path, "/wallet-one/balances"):
				if query.Get("includeAll") != "true" {
					t.Fatal("balance filter was dropped")
				}
				switch query.Get("pageAfter") {
				case "":
					payload = circleReadTestPage(t, "tokenBalances", []any{circleReadTestBalance("native-token", "0", true)})
					next = circleReadTestNext(request, "opaque-balance-next")
				case "opaque-balance-next":
					secondBalanceAttempts++
					if secondBalanceAttempts == 1 {
						return paymentReadTestResponse(503, "temporary", &closed), nil
					}
					payload = circleReadTestPage(t, "tokenBalances", []any{circleReadTestBalance("usdc-token", "1.000001", false)})
				default:
					t.Fatal("balance cursor was inferred from token id", query)
				}
			case strings.HasSuffix(request.URL.Path, "/wallet-two/balances"):
				payload = circleReadTestPage(t, "tokenBalances", []any{})
			default:
				t.Fatal("unexpected page", request.URL.Path)
			}
			response := paymentReadTestResponse(200, payload, &closed)
			response.Header = http.Header{"Link": {"<" + request.URL.String() + ">; rel=\"self\"; title=\"a,b\""}}
			if next != "" {
				response.Header.Add("Link", next+", <"+request.URL.String()+">; rel=\"first\"")
			}
			return response, nil
		},
	})
	wallets, err := circleReadTestList(t, client, context.Background(), &tokens)
	if err != nil || len(wallets) != 2 || wallets[0].TokenId != "usdc-token" || wallets[0].BalanceUsdcNanoCents != model.UsdToNanoCents(1.000001) || wallets[1].BalanceUsdcNanoCents != 0 || wallets[1].Blockchain != "Synthetic native" || calls != 6 || closed != calls || tokens != 1 || waits != 1 {
		t.Fatalf("incomplete or speculative wallet census: %+v err=%v calls=%d closed=%d tokens=%d waits=%d", wallets, err, calls, closed, tokens, waits)
	}
}

func TestCircleReadPaginationRefusesCredentialEscapeAndCycles(t *testing.T) {
	for _, defect := range []string{"host", "scheme", "path", "user", "fragment", "filter", "lost-filter", "size", "duplicate-next", "duplicate-query", "cycle", "malformed", "oversized"} {
		calls, closed, tokens := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
			requirePaymentReadRequest(t, request)
			calls++
			if strings.HasSuffix(request.URL.Path, "/wallets") {
				return paymentReadTestResponse(200, circleReadTestPage(t, "wallets", []any{circleReadTestWallet("wallet-one")}), &closed), nil
			}
			uri := *request.URL
			query := uri.Query()
			query.Set("pageAfter", "opaque-next")
			uri.RawQuery = query.Encode()
			next := circleReadTestNext(request, "opaque-next")
			switch defect {
			case "host":
				uri.Host = "different.example"
			case "scheme":
				uri.Scheme = "http"
			case "path":
				uri.Path = "/different-endpoint"
			case "user":
				next = strings.Replace(next, "https://", "https://synthetic-user@", 1)
			case "fragment":
				uri.Fragment = "different-view"
			case "filter":
				query.Set("includeAll", "false")
			case "lost-filter":
				query.Del("includeAll")
			case "size":
				query.Set("pageSize", "10000")
			case "duplicate-query":
				query.Add("pageAfter", "other")
			case "duplicate-next":
				next += ", " + next
			case "malformed":
				next = "not-a-link"
			case "oversized":
				next = strings.Repeat("x", 16*1024+1)
			case "cycle":
				// The second page points back to its own opaque next cursor.
			}
			if defect != "user" && defect != "duplicate-next" && defect != "malformed" && defect != "oversized" {
				uri.RawQuery = query.Encode()
				next = "<" + uri.String() + ">; rel=\"next\""
			}
			response := paymentReadTestResponse(200, circleReadTestPage(t, "tokenBalances", []any{}), &closed)
			response.Header = http.Header{"Link": {next}}
			return response, nil
		}})
		wallets, err := circleReadTestList(t, client, context.Background(), &tokens)
		expectedCalls := 2
		if defect == "cycle" {
			expectedCalls = 3
		}
		if err == nil || wallets != nil || calls != expectedCalls || closed != calls || tokens != 1 {
			t.Fatal("unsafe pagination escaped or looked complete", defect, err, calls, closed, tokens)
		}
	}
}

func TestCircleReadMalformedBalancesNeverBecomeZero(t *testing.T) {
	for _, defect := range []string{"absent", "null", "object", "amount", "overflow", "precision", "native", "symbol", "chain", "duplicate"} {
		calls, closed, tokens, waits := 0, 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") },
			do: func(request *http.Request) (*http.Response, error) {
				calls++
				if strings.HasSuffix(request.URL.Path, "/wallets") {
					return paymentReadTestResponse(200, circleReadTestPage(t, "wallets", []any{circleReadTestWallet("wallet-one")}), &closed), nil
				}
				balance := circleReadTestBalance("usdc-token", "1", false)
				token := balance["token"].(map[string]any)
				var items any = []any{balance}
				switch defect {
				case "absent":
					return paymentReadTestResponse(200, `{"data":{}}`, &closed), nil
				case "null":
					items = nil
				case "object":
					items = map[string]any{}
				case "amount":
					balance["amount"] = "NaN"
				case "overflow":
					balance["amount"] = "9999999999999999999999"
				case "precision":
					balance["amount"] = "0.0000000001"
				case "native":
					delete(token, "isNative")
				case "symbol":
					delete(token, "symbol")
				case "chain":
					token["blockchain"] = "DIFFERENT"
				case "duplicate":
					items = []any{balance, balance}
				}
				return paymentReadTestResponse(200, circleReadTestPage(t, "tokenBalances", items), &closed), nil
			},
		})
		wallets, err := circleReadTestList(t, client, context.Background(), &tokens)
		if err == nil || wallets != nil || calls != 2 || closed != calls || waits != 0 || tokens != 1 {
			t.Fatal("malformed balance became zero or retried", defect, err, calls, closed, waits)
		}
	}
}

func TestCircleReadWalletIdentityAppliesToEveryListMember(t *testing.T) {
	for _, defect := range []string{"missing-address", "missing-date", "foreign-user", "duplicate", "null", "missing-array"} {
		calls, closed, tokens := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
			calls++
			wallet := circleReadTestWallet("wallet-one")
			items := []any{wallet}
			switch defect {
			case "missing-address":
				delete(wallet, "address")
			case "missing-date":
				delete(wallet, "createDate")
			case "foreign-user":
				wallet["userId"] = "different-user"
			case "duplicate":
				items = append(items, wallet)
			case "null":
				items = []any{nil}
			case "missing-array":
				return paymentReadTestResponse(200, `{"data":{}}`, &closed), nil
			}
			return paymentReadTestResponse(200, circleReadTestPage(t, "wallets", items), &closed), nil
		}})
		wallets, err := circleReadTestList(t, client, context.Background(), &tokens)
		if err == nil || wallets != nil || calls != 1 || closed != 1 || tokens != 1 {
			t.Fatal("invalid list member was silently skipped", defect, err, calls, closed)
		}
	}
}

func TestCircleReadCompositeOperationSharesOneBudget(t *testing.T) {
	started := time.Now()
	now := started
	calls, closed, tokens, waits := 0, 0, 0, 0
	client := paymentReadTestCircle(paymentReadHooks{
		now: func() time.Time { return now },
		wait: func(ctx context.Context, delay time.Duration) error {
			waits++
			if delay != 50*time.Second || calls != closed {
				t.Fatal("nested balance read restarted the budget or lost body ownership", delay)
			}
			now = now.Add(delay)
			return ctx.Err()
		},
		do: func(request *http.Request) (*http.Response, error) {
			requirePaymentReadRequest(t, request)
			calls++
			if calls == 1 {
				now = now.Add(40 * time.Second)
				return paymentReadTestResponse(200, circleReadTestPage(t, "wallets", []any{circleReadTestWallet("wallet-one")}), &closed), nil
			}
			response := paymentReadTestResponse(503, "temporary", &closed)
			response.Header = http.Header{"Retry-After": {"60"}}
			return response, nil
		},
	})
	wallets, err := client.findCircleWallets(&session.ClientSession{Ctx: context.Background()}, func(owner *session.ClientSession) (*CircleUserToken, error) {
		tokens++
		now = now.Add(210 * time.Second)
		return &CircleUserToken{circleUserId: server.NewId(), UserToken: "synthetic-user-token"}, nil
	}, "Synthetic", "SYNTHETIC")
	if wallets != nil || !errors.Is(err, context.DeadlineExceeded) || calls != 2 || closed != 2 || tokens != 1 || waits != 1 || now.Sub(started) != 300*time.Second {
		t.Fatal("composite operation multiplied the owned budget", err, calls, closed, tokens, waits, now.Sub(started))
	}
}

func TestCircleReadEarlierCallerDeadlineBoundsComposite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	started := time.Now()
	now := started
	calls, closed, tokens, waits := 0, 0, 0, 0
	client := paymentReadTestCircle(paymentReadHooks{
		now: func() time.Time { return now },
		wait: func(ctx context.Context, delay time.Duration) error {
			waits++
			if delay <= 0 || delay > 25*time.Second {
				t.Fatal("earlier deadline was extended", delay)
			}
			now = now.Add(delay)
			return ctx.Err()
		},
		do: func(request *http.Request) (*http.Response, error) {
			calls++
			response := paymentReadTestResponse(503, "temporary", &closed)
			response.Header = http.Header{"Retry-After": {"300"}}
			return response, nil
		},
	})
	wallets, err := circleReadTestList(t, client, ctx, &tokens)
	if wallets != nil || !errors.Is(err, context.DeadlineExceeded) || calls != 1 || closed != 1 || tokens != 1 || waits != 1 || now.Sub(started) > 25*time.Second {
		t.Fatal("earlier caller deadline lost", err, calls, closed, tokens, waits)
	}
}

func TestCircleReadBalanceCancellationJoinsAndDiscardsPartialList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, released := make(chan struct{}), make(chan struct{})
	calls, closed, tokens := 0, 0, 0
	client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return paymentReadTestResponse(200, circleReadTestPage(t, "wallets", []any{circleReadTestWallet("wallet-one")}), &closed), nil
		}
		close(entered)
		<-released
		response := paymentReadTestResponse(200, circleReadTestPage(t, "tokenBalances", []any{}), &closed)
		response.Body.(*paymentReadTestBody).afterRead = cancel
		return response, nil
	}})
	done := make(chan error, 1)
	go func() {
		wallets, err := circleReadTestList(t, client, ctx, &tokens)
		if wallets != nil {
			err = errors.Join(err, errors.New("canceled partial wallet list was exposed"))
		}
		done <- err
	}()
	awaitPaymentReadBarrier(t, entered)
	close(released)
	err := awaitPaymentReadResult(t, done)
	if !errors.Is(err, context.Canceled) || calls != 2 || closed != 2 || tokens != 1 {
		t.Fatal("canceled body did not join", err, calls, closed, tokens)
	}
}

func TestCircleReadCanceledAdmissionDoesNotLoadConfigOrCreateToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &CoreCircleApiClient{}
	if key, err := client.getPublicKey(ctx); key != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled entity admission read configuration", err)
	}
	if err := client.verifyCircleAuth(ctx, "synthetic-key", "synthetic-signature", nil); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled notification admission read configuration", err)
	}
	if wallet, err := client.getCircleWallet(ctx, "synthetic-wallet"); wallet != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled wallet admission read configuration", err)
	}
	tokens := 0
	if wallets, err := circleReadTestList(t, client, ctx, &tokens); wallets != nil || !errors.Is(err, context.Canceled) || tokens != 0 {
		t.Fatal("canceled list created a user token", err, tokens)
	}
}

func TestCircleReadCensusCapacityIsBounded(t *testing.T) {
	for _, limit := range []string{"page", "wallets", "tokens", "empty-next-pages"} {
		calls, closed, tokens := 0, 0, 0
		walletCount, tokenCount := 0, 0
		client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
			calls++
			key := "wallets"
			items := []any{}
			if limit == "page" {
				for i := 0; i <= circleReadPageSize; i++ {
					items = append(items, circleReadTestWallet(fmt.Sprint("wallet-", i)))
				}
			} else if limit == "wallets" {
				for i := 0; i < circleReadPageSize; i++ {
					walletCount++
					items = append(items, circleReadTestWallet(fmt.Sprint("wallet-", walletCount)))
				}
			} else if limit == "tokens" {
				if strings.HasSuffix(request.URL.Path, "/wallets") {
					return paymentReadTestResponse(200, circleReadTestPage(t, "wallets", []any{circleReadTestWallet("wallet-one")}), &closed), nil
				}
				key = "tokenBalances"
				for i := 0; i < circleReadPageSize; i++ {
					tokenCount++
					balance := circleReadTestBalance(fmt.Sprint("token-", tokenCount), "1", false)
					balance["token"].(map[string]any)["symbol"] = "SYN"
					items = append(items, balance)
				}
			}
			response := paymentReadTestResponse(200, circleReadTestPage(t, key, items), &closed)
			response.Header = http.Header{"Link": {circleReadTestNext(request, fmt.Sprint("opaque-page-", calls))}}
			return response, nil
		}})
		wallets, err := circleReadTestList(t, client, context.Background(), &tokens)
		expected := map[string]int{"page": 1, "wallets": 6, "tokens": 12, "empty-next-pages": maximumCircleObservationPages}[limit]
		if wallets != nil || err == nil || calls != expected || closed != calls || tokens != 1 {
			t.Fatal("census exceeded bounded work or reported partial success", limit, err, calls, closed, tokens)
		}
	}
}

func TestPaymentReadRetryAfterBoundsAndTransientInternalError(t *testing.T) {
	for _, header := range []string{"65", "date", "99999999999999999999999999999999999", "invalid", "-1"} {
		started := time.Now().Truncate(time.Second)
		now := started
		calls, closed, waits := 0, 0, 0
		var waited time.Duration
		client := paymentReadTestCircle(paymentReadHooks{
			now: func() time.Time { return now },
			wait: func(ctx context.Context, delay time.Duration) error {
				waits++
				waited = delay
				now = now.Add(delay)
				return ctx.Err()
			},
			do: func(request *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					return paymentReadTestResponse(200, paymentReadTestPayload(paymentReadTestId), &closed), nil
				}
				response := paymentReadTestResponse(500, "temporary", &closed)
				value := header
				if header == "date" {
					value = started.Add(65 * time.Second).UTC().Format(http.TimeFormat)
				}
				response.Header = http.Header{"Retry-After": {value}}
				return response, nil
			},
		})
		result, err := client.GetTransaction(context.Background(), paymentReadTestId)
		if strings.HasPrefix(header, "999") {
			if result != nil || !errors.Is(err, context.DeadlineExceeded) || calls != 1 || waited != 300*time.Second {
				t.Fatal("oversized retry-after overflowed or extended the owner", err, calls, waited)
			}
		} else {
			if err != nil || result == nil || calls != 2 {
				t.Fatal("transient 500 did not recover", header, err, calls)
			}
			if (header == "65" || header == "date") && waited != 65*time.Second {
				t.Fatal("valid retry-after ignored", header, waited)
			}
			if (header == "invalid" || header == "-1") && (waited < time.Second || waited >= 2*time.Second) {
				t.Fatal("invalid retry-after bypassed bounded pause", header, waited)
			}
		}
		if closed != calls || waits != 1 {
			t.Fatal("retry-after lost body/join ownership", closed, calls, waits)
		}
	}
}

type circleReadTestTransport struct {
	do func(*http.Request) (*http.Response, error)
}

func (self *circleReadTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self.do(request)
}

func TestPaymentReadRedirectCannotForwardCredentials(t *testing.T) {
	calls, closed := 0, 0
	transportClient := paymentReadHttpClient()
	transportClient.CloseIdleConnections()
	transportClient.Transport = &circleReadTestTransport{do: func(request *http.Request) (*http.Response, error) {
		calls++
		if calls != 1 || request.URL.Host != "read.example" {
			t.Fatal("redirect received the authenticated request")
		}
		response := paymentReadTestResponse(302, "redirect refused", &closed)
		response.Header = http.Header{"Location": {"https://different.example/observe"}}
		return response, nil
	}}
	_, err := paymentHttpGet(context.Background(), PaymentReadSettings{}, paymentReadHooks{do: transportClient.Do}, "https://read.example/observe", func(header http.Header) {
		header.Set("Authorization", "Bearer synthetic-token")
		header.Set("X-User-Token", "synthetic-user-token")
	}, func(*http.Response, []byte) (string, error) { return "unexpected", nil })
	if err == nil || calls != 1 || closed != 1 || errors.Is(err, io.EOF) {
		t.Fatal("redirect was followed or treated as a transport outage", err, calls, closed)
	}
}
