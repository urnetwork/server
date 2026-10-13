// These ledger reads use the real request/decoder paths with owned synthetic
// transports. No test reaches Circle or loads the Circle configuration.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

const circlePayoutLedgerTestWallet = "synthetic-wallet"
const circlePayoutLedgerTestToken = "0xabcdef0123456789abcdef0123456789abcdef01"

var circlePayoutLedgerTestFrom = time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

func circlePayoutLedgerTestTransaction(id, state, createDate string) map[string]any {
	return map[string]any{"id": id, "walletId": circlePayoutLedgerTestWallet, "state": state, "createDate": createDate, "txHash": "synthetic-hash-" + id, "destinationAddress": "synthetic-destination", "transactionType": "OUTBOUND", "operation": "TRANSFER", "amounts": []string{"1.5"}}
}

func circlePayoutLedgerTestBalance(id, address, amount string) map[string]any {
	token := map[string]any{"id": id, "blockchain": "SYNTHETIC", "isNative": address == "", "symbol": "SYN"}
	if address != "" {
		token["tokenAddress"] = address
	}
	return map[string]any{"amount": amount, "token": token, "updateDate": "2026-10-12T00:00:00Z"}
}

// A documented Circle page: self and first links always, and `next` (a whole
// Link value) only before the last page.
func circlePayoutLedgerTestPayload(request *http.Request, payload string, next string, closed *int) *http.Response {
	response := paymentReadTestResponse(http.StatusOK, payload, closed)
	firstPage := *request.URL
	query := firstPage.Query()
	query.Del("pageAfter")
	firstPage.RawQuery = query.Encode()
	response.Header = http.Header{"Link": {"<" + request.URL.String() + ">; rel=\"self\", <" + firstPage.String() + ">; rel=\"first\""}}
	if next != "" {
		response.Header.Add("Link", next)
	}
	return response
}

func circlePayoutLedgerTestResponse(t *testing.T, request *http.Request, key string, items any, next string, closed *int) *http.Response {
	t.Helper()
	return circlePayoutLedgerTestPayload(request, circleReadTestPage(t, key, items), next, closed)
}

// Exactly the documented endpoint, one value per filter, a cursor only after
// the first page, the API key and no user token.
func requireCirclePayoutLedgerRequest(t *testing.T, request *http.Request, path string, filters map[string]string) {
	t.Helper()
	requirePaymentReadRequest(t, request)
	query := request.URL.Query()
	keys := []string{}
	for key, values := range query {
		if len(values) != 1 || (key != "pageAfter" && filters[key] != values[0]) {
			t.Fatalf("request filter %s changed: %v", key, values)
		}
		keys = append(keys, key)
	}
	expected := []string{}
	for key := range filters {
		expected = append(expected, key)
	}
	if query.Has("pageAfter") {
		expected = append(expected, "pageAfter")
	}
	slices.Sort(keys)
	slices.Sort(expected)
	if request.URL.Scheme != "https" || request.URL.Host != "api.circle.com" || request.URL.Path != path || !slices.Equal(keys, expected) ||
		request.Header.Get("Authorization") != "Bearer synthetic-read-token" || request.Header.Get("X-User-Token") != "" {
		t.Fatalf("request endpoint, filters or credentials changed: %s %v", request.URL, request.Header)
	}
}

func requireCirclePayoutLedgerListingRequest(t *testing.T, request *http.Request, from string) {
	t.Helper()
	requireCirclePayoutLedgerRequest(t, request, "/v1/w3s/transactions", map[string]string{"walletIds": circlePayoutLedgerTestWallet, "from": from, "includeAll": "true", "pageSize": "50"})
}

func requireCirclePayoutLedgerBalanceRequest(t *testing.T, request *http.Request) {
	t.Helper()
	requireCirclePayoutLedgerRequest(t, request, "/v1/w3s/wallets/"+circlePayoutLedgerTestWallet+"/balances", map[string]string{"includeAll": "true", "pageSize": "50"})
}

// Each refusal case must fail with its own cause, not an incidental error.
func requireCirclePayoutLedgerRefusal(t *testing.T, defect string, err error, expected string) {
	t.Helper()
	if expected == "" || err == nil || !strings.Contains(err.Error(), expected) {
		t.Fatalf("%s: expected refusal %q, got %v", defect, expected, err)
	}
}

func circlePayoutLedgerTestIds(transactions []CircleTransaction) []string {
	ids := []string{}
	for _, transaction := range transactions {
		ids = append(ids, transaction.Id)
	}
	return ids
}

// RC-5 Every page is followed by its response Link, through an empty page and
// a rate-limited retry, and every state is listed with no state filter.
func TestCirclePayoutLedgerListsEveryPageAndState(t *testing.T) {
	calls, closed, tokens, waits, limited := 0, 0, 0, 0, false
	var waited time.Duration
	client := paymentReadTestCircle(paymentReadHooks{
		wait: func(ctx context.Context, delay time.Duration) error {
			waits++
			waited = delay
			return ctx.Err()
		},
		do: func(request *http.Request) (*http.Response, error) {
			requireCirclePayoutLedgerListingRequest(t, request, "2026-08-01T10:00:00Z")
			if closed != calls {
				t.Fatal("prior body was not closed")
			}
			calls++
			switch request.URL.Query().Get("pageAfter") {
			case "":
				return circlePayoutLedgerTestResponse(t, request, "transactions", []any{
					circlePayoutLedgerTestTransaction("transaction-sent", "SENT", "2026-10-11T00:00:00Z"),
					circlePayoutLedgerTestTransaction("transaction-failed", "FAILED", "2026-10-10T00:00:00.123Z"),
				}, circleReadTestNext(request, "opaque-second"), &closed), nil
			case "opaque-second":
				if !limited {
					limited = true
					response := paymentReadTestResponse(http.StatusTooManyRequests, "rate limited", &closed)
					response.Header = http.Header{"Retry-After": {"2"}}
					return response, nil
				}
				return circlePayoutLedgerTestResponse(t, request, "transactions", []any{}, circleReadTestNext(request, "opaque-third"), &closed), nil
			case "opaque-third":
				return circlePayoutLedgerTestResponse(t, request, "transactions", []any{
					circlePayoutLedgerTestTransaction("transaction-denied", "DENIED", "2026-09-01T00:00:00Z"),
					circlePayoutLedgerTestTransaction("transaction-cancelled", "CANCELLED", "2026-08-15T00:00:00Z"),
					circlePayoutLedgerTestTransaction("transaction-complete", "COMPLETE", "2026-08-01T10:00:00Z"),
				}, "", &closed), nil
			}
			t.Fatal("cursor was not taken from the response Link", request.URL)
			return nil, nil
		},
	})
	client.readToken = func() string {
		tokens++
		return "synthetic-read-token"
	}
	transactions, err := client.ListWalletTransactions(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestFrom)
	expected := []string{"transaction-sent", "transaction-failed", "transaction-denied", "transaction-cancelled", "transaction-complete"}
	if err != nil || !slices.Equal(circlePayoutLedgerTestIds(transactions), expected) || calls != 4 || closed != calls || tokens != 1 || waits != 1 || waited != 2*time.Second {
		t.Fatalf("listing was short, speculative or lost the retry: %v err=%v calls=%d closed=%d tokens=%d waits=%d waited=%v", circlePayoutLedgerTestIds(transactions), err, calls, closed, tokens, waits, waited)
	}
	failed := transactions[1]
	if failed.State != "FAILED" || failed.WalletId != circlePayoutLedgerTestWallet || failed.CreateDate != "2026-10-10T00:00:00.123Z" || failed.TxHash != "synthetic-hash-transaction-failed" ||
		failed.DestinationAddress != "synthetic-destination" || failed.TransactionType != "OUTBOUND" || failed.Operation != "TRANSFER" || !slices.Equal(failed.Amounts, []string{"1.5"}) {
		t.Fatalf("listed transaction lost its fields: %+v", failed)
	}
}

// RC-5 A wallet without transactions since `from` lists as empty, not as an
// error.
func TestCirclePayoutLedgerListingOfQuietWalletIsEmpty(t *testing.T) {
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
		requireCirclePayoutLedgerListingRequest(t, request, "2026-08-01T10:00:00Z")
		calls++
		return circlePayoutLedgerTestResponse(t, request, "transactions", []any{}, "", &closed), nil
	}})
	transactions, err := client.ListWalletTransactions(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestFrom)
	if err != nil || transactions == nil || len(transactions) != 0 || calls != 1 || closed != 1 {
		t.Fatal("quiet wallet did not list as empty", transactions, err, calls, closed)
	}
}

// RC-5 One read budget cannot span the payout history: each page has its own,
// so a listing longer than one budget still completes.
func TestCirclePayoutLedgerListingBudgetsEachPage(t *testing.T) {
	started := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	now := started
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{
		now:  func() time.Time { return now },
		wait: func(context.Context, time.Duration) error { return errors.New("unexpected retry") },
		do: func(request *http.Request) (*http.Response, error) {
			calls++
			now = now.Add(200 * time.Second)
			next := ""
			if calls < 3 {
				next = circleReadTestNext(request, fmt.Sprint("opaque-", calls))
			}
			return circlePayoutLedgerTestResponse(t, request, "transactions", []any{circlePayoutLedgerTestTransaction(fmt.Sprint("transaction-", calls), "COMPLETE", "2026-09-01T00:00:00Z")}, next, &closed), nil
		},
	})
	transactions, err := client.ListWalletTransactions(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestFrom)
	if err != nil || !slices.Equal(circlePayoutLedgerTestIds(transactions), []string{"transaction-1", "transaction-2", "transaction-3"}) || calls != 3 || closed != calls || now.Sub(started) != 600*time.Second {
		t.Fatalf("listing was bounded by one read budget: %v err=%v calls=%d elapsed=%v", circlePayoutLedgerTestIds(transactions), err, calls, now.Sub(started))
	}
}

// RC-5 Circle receives `from` as the floored UTC second and treats it as
// inclusive; transactions in that second before `from` are not returned.
func TestCirclePayoutLedgerListingAppliesFromInclusively(t *testing.T) {
	from := time.Date(2026, 8, 1, 12, 0, 0, 500_000_000, time.FixedZone("synthetic", 2*60*60))
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
		requireCirclePayoutLedgerListingRequest(t, request, "2026-08-01T10:00:00Z")
		calls++
		return circlePayoutLedgerTestResponse(t, request, "transactions", []any{
			circlePayoutLedgerTestTransaction("transaction-later", "COMPLETE", "2026-08-01T10:00:01Z"),
			circlePayoutLedgerTestTransaction("transaction-at-from", "COMPLETE", "2026-08-01T10:00:00.5Z"),
			circlePayoutLedgerTestTransaction("transaction-offset", "COMPLETE", "2026-08-01T12:00:00.75+02:00"),
			circlePayoutLedgerTestTransaction("transaction-floored-second", "COMPLETE", "2026-08-01T10:00:00Z"),
			circlePayoutLedgerTestTransaction("transaction-before-from", "COMPLETE", "2026-08-01T10:00:00.499Z"),
		}, "", &closed), nil
	}})
	transactions, err := client.ListWalletTransactions(context.Background(), circlePayoutLedgerTestWallet, from)
	if err != nil || !slices.Equal(circlePayoutLedgerTestIds(transactions), []string{"transaction-later", "transaction-at-from", "transaction-offset"}) || calls != 1 || closed != 1 {
		t.Fatalf("listing misapplied from: %v err=%v calls=%d", circlePayoutLedgerTestIds(transactions), err, calls)
	}
}

// RC-5 One invalid, repeated, foreign or early transaction fails the whole
// listing; the valid pages before it are discarded, never returned short.
func TestCirclePayoutLedgerListingRefusesInvalidTransactions(t *testing.T) {
	for _, defect := range []string{"foreign-wallet", "missing-wallet", "early", "unparseable-date", "zoneless-date", "missing-date", "missing-id", "invalid-id", "null", "duplicate", "duplicate-across-pages"} {
		calls, closed, waits := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") },
			do: func(request *http.Request) (*http.Response, error) {
				calls++
				valid := circlePayoutLedgerTestTransaction("transaction-valid", "COMPLETE", "2026-08-02T00:00:00Z")
				if calls == 1 {
					return circlePayoutLedgerTestResponse(t, request, "transactions", []any{valid}, circleReadTestNext(request, "opaque-next"), &closed), nil
				}
				item := circlePayoutLedgerTestTransaction("transaction-defect", "COMPLETE", "2026-08-02T00:00:00Z")
				items := []any{item}
				switch defect {
				case "foreign-wallet":
					item["walletId"] = "different-wallet"
				case "missing-wallet":
					delete(item, "walletId")
				case "early":
					item["createDate"] = "2026-08-01T09:59:59Z"
				case "unparseable-date":
					item["createDate"] = "2026-08-02 00:00:00"
				case "zoneless-date":
					item["createDate"] = "2026-08-02T00:00:00"
				case "missing-date":
					delete(item, "createDate")
				case "missing-id":
					delete(item, "id")
				case "invalid-id":
					item["id"] = "synthetic/transaction"
				case "null":
					items = []any{nil}
				case "duplicate":
					items = []any{item, item}
				case "duplicate-across-pages":
					items = []any{valid}
				}
				return circlePayoutLedgerTestResponse(t, request, "transactions", items, "", &closed), nil
			},
		})
		transactions, err := client.ListWalletTransactions(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestFrom)
		id, ok := map[string]string{"missing-id": "", "null": "", "invalid-id": "synthetic/transaction", "duplicate-across-pages": "transaction-valid"}[defect]
		if !ok {
			id = "transaction-defect"
		}
		requireCirclePayoutLedgerRefusal(t, defect, err, fmt.Sprintf("Circle wallet %s listing has an invalid, repeated, foreign or early transaction %q", circlePayoutLedgerTestWallet, id))
		if transactions != nil || calls != 2 || closed != calls || waits != 0 {
			t.Fatal("invalid listed transaction was accepted, skipped or retried", defect, err, calls, closed, waits)
		}
	}
}

// RC-5 Circle may omit our fixed filters from a next link, wholly or in part:
// the link's cursor and page size are taken and every filter of ours is
// re-applied, so each page is requested under the first page's filters.
func TestCirclePayoutLedgerListingReappliesOmittedFilters(t *testing.T) {
	pages := map[string]struct{ pageSize, next string }{
		// The documented shape: only the cursor and page size.
		"": {"50", "<https://api.circle.com/v1/w3s/transactions?pageAfter=opaque-second&pageSize=50>; rel=\"next\""},
		// One filter repeated unchanged, the others omitted, a smaller page.
		"opaque-second": {"50", "<https://api.circle.com/v1/w3s/transactions?pageAfter=opaque-third&pageSize=20&walletIds=" + circlePayoutLedgerTestWallet + ">; rel=\"next\""},
		// Only the cursor: our own page size is re-applied.
		"opaque-third":  {"20", "<https://api.circle.com/v1/w3s/transactions?pageAfter=opaque-fourth>; rel=\"next\""},
		"opaque-fourth": {"50", ""},
	}
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
		calls++
		page, ok := pages[request.URL.Query().Get("pageAfter")]
		if !ok {
			t.Fatal("cursor was not taken from the response Link", request.URL)
		}
		requireCirclePayoutLedgerRequest(t, request, "/v1/w3s/transactions", map[string]string{"walletIds": circlePayoutLedgerTestWallet, "from": "2026-08-01T10:00:00Z", "includeAll": "true", "pageSize": page.pageSize})
		return circlePayoutLedgerTestResponse(t, request, "transactions", []any{circlePayoutLedgerTestTransaction(fmt.Sprint("transaction-", calls), "COMPLETE", "2026-09-01T00:00:00Z")}, page.next, &closed), nil
	}})
	transactions, err := client.ListWalletTransactions(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestFrom)
	if err != nil || !slices.Equal(circlePayoutLedgerTestIds(transactions), []string{"transaction-1", "transaction-2", "transaction-3", "transaction-4"}) || calls != 4 || closed != calls {
		t.Fatalf("omitted filters were not re-applied: %v err=%v calls=%d closed=%d", circlePayoutLedgerTestIds(transactions), err, calls, closed)
	}
}

// RC-5 A page without the documented Link, a cursor cycle, or a next link
// that changes the endpoint, the page bound or a filter it carries, adds a
// filter, or holds both or neither cursor fails the listing before any
// further page is requested, whether the link repeats our filters or omits
// them.
func TestCirclePayoutLedgerListingRefusesUnsafePagination(t *testing.T) {
	refusals := map[string]string{
		"missing-link":   "transactions page has no pagination Link",
		"cycle":          "transactions pagination repeated a page",
		"changed-from":   "pagination changed fixed filters",
		"changed-wallet": "pagination changed fixed filters",
		"foreign-filter": "pagination changed fixed filters",
		"host":           "pagination changed the authenticated endpoint",
		"page-size":      "pagination changed the page bound",
		"both-cursors":   "pagination requires one opaque cursor",
		"no-cursor":      "pagination requires one opaque cursor",
	}
	for _, shape := range []string{"repeated", "omitted"} {
		for _, defect := range []string{"missing-link", "cycle", "changed-from", "changed-wallet", "foreign-filter", "host", "page-size", "both-cursors", "no-cursor"} {
			calls, closed := 0, 0
			client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
				calls++
				items := []any{circlePayoutLedgerTestTransaction(fmt.Sprint("transaction-", calls), "COMPLETE", "2026-09-01T00:00:00Z")}
				if defect == "missing-link" {
					return paymentReadTestResponse(http.StatusOK, circleReadTestPage(t, "transactions", items), &closed), nil
				}
				uri := *request.URL
				query := url.Values{"pageSize": {"50"}}
				if shape == "repeated" {
					query = uri.Query()
				}
				query.Set("pageAfter", "opaque-next")
				switch defect {
				case "cycle":
					// The second page links to itself.
				case "changed-from":
					query.Set("from", "2026-07-01T00:00:00Z")
				case "changed-wallet":
					query.Set("walletIds", "different-wallet")
				case "foreign-filter":
					query.Set("order", "ASC")
				case "host":
					uri.Host = "different.example"
				case "page-size":
					query.Set("pageSize", "51")
				case "both-cursors":
					query.Set("pageBefore", "opaque-previous")
				case "no-cursor":
					query.Del("pageAfter")
				}
				uri.RawQuery = query.Encode()
				return circlePayoutLedgerTestResponse(t, request, "transactions", items, "<"+uri.String()+">; rel=\"next\"", &closed), nil
			}})
			transactions, err := client.ListWalletTransactions(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestFrom)
			expectedCalls := 1
			if defect == "cycle" {
				expectedCalls = 2
			}
			requireCirclePayoutLedgerRefusal(t, shape+"/"+defect, err, refusals[defect])
			if transactions != nil || calls != expectedCalls || closed != calls {
				t.Fatal("unsafe pagination was followed or looked complete", shape, defect, err, calls, closed)
			}
		}
	}
}

// RC-5 More pages than the bound is an error, never a truncated listing.
func TestCirclePayoutLedgerListingPageBoundIsAnError(t *testing.T) {
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
		calls++
		next := fmt.Sprintf("<https://api.circle.com/v1/w3s/transactions?pageAfter=opaque-%d&pageSize=50>; rel=\"next\"", calls)
		return circlePayoutLedgerTestPayload(request, `{"data":{"transactions":[]}}`, next, &closed), nil
	}})
	transactions, err := client.ListWalletTransactions(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestFrom)
	requireCirclePayoutLedgerRefusal(t, "page-bound", err, "transactions exceed 50000 pages")
	if transactions != nil || calls != maximumCirclePayoutLedgerPages || closed != calls {
		t.Fatal("listing beyond the page bound looked complete", err, calls, closed)
	}
}

// RC-5 A refused GET or a malformed page fails the listing without a retry.
func TestCirclePayoutLedgerListingRefusesHttpAndJsonErrors(t *testing.T) {
	refusals := map[string]string{
		"bad-request":   "Bad status: 400",
		"unauthorized":  "Bad status: 401",
		"malformed":     "invalid character",
		"missing-array": "Circle GET is missing transactions",
		"null-array":    "Circle GET is missing transactions",
		"object":        "Circle GET transactions is not an array",
		"oversized":     "Circle GET transactions exceeds requested page bound",
		"field-type":    "cannot unmarshal",
	}
	for _, defect := range []string{"bad-request", "unauthorized", "malformed", "missing-array", "null-array", "object", "oversized", "field-type"} {
		calls, closed, waits := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") },
			do: func(request *http.Request) (*http.Response, error) {
				calls++
				payload := ""
				switch defect {
				case "bad-request":
					return paymentReadTestResponse(http.StatusBadRequest, `{"code":2,"message":"synthetic invalid request"}`, &closed), nil
				case "unauthorized":
					return paymentReadTestResponse(http.StatusUnauthorized, `{"code":401,"message":"synthetic unauthorized"}`, &closed), nil
				case "malformed":
					payload = "synthetic-not-json"
				case "missing-array":
					payload = `{"data":{}}`
				case "null-array":
					payload = `{"data":{"transactions":null}}`
				case "object":
					payload = `{"data":{"transactions":{}}}`
				case "oversized":
					items := []any{}
					for i := 0; i <= circleReadPageSize; i++ {
						items = append(items, circlePayoutLedgerTestTransaction(fmt.Sprint("transaction-", i), "COMPLETE", "2026-09-01T00:00:00Z"))
					}
					payload = circleReadTestPage(t, "transactions", items)
				case "field-type":
					item := circlePayoutLedgerTestTransaction("transaction-defect", "COMPLETE", "2026-09-01T00:00:00Z")
					item["amounts"] = "synthetic-not-an-array"
					payload = circleReadTestPage(t, "transactions", []any{item})
				}
				return circlePayoutLedgerTestPayload(request, payload, "", &closed), nil
			},
		})
		transactions, err := client.ListWalletTransactions(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestFrom)
		requireCirclePayoutLedgerRefusal(t, defect, err, refusals[defect])
		if transactions != nil || calls != 1 || closed != 1 || waits != 0 {
			t.Fatal("refused or malformed page was accepted or retried", defect, err, calls, closed, waits)
		}
	}
}

// RC-5 Invalid arguments are refused before the configuration is read or any
// GET is made; a comma would make the wallet filter name several wallets.
func TestCirclePayoutLedgerListingRefusesInvalidArguments(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var missing context.Context
	for _, defect := range []string{"nil-context", "canceled", "empty-wallet", "wallet-list", "wallet-path", "zero-from"} {
		calls, tokens := 0, 0
		client := &CoreCircleApiClient{
			readHooks: paymentReadHooks{do: func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") }},
			readToken: func() string { tokens++; return "synthetic-read-token" },
		}
		ctx, wallet, from := context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestFrom
		switch defect {
		case "nil-context":
			ctx = missing
		case "canceled":
			ctx = canceled
		case "empty-wallet":
			wallet = ""
		case "wallet-list":
			wallet = circlePayoutLedgerTestWallet + ",different-wallet"
		case "wallet-path":
			wallet = "synthetic/wallet"
		case "zero-from":
			from = time.Time{}
		}
		transactions, err := client.ListWalletTransactions(ctx, wallet, from)
		expected := map[string]string{"nil-context": "listing requires a context", "canceled": context.Canceled.Error()}[defect]
		if expected == "" {
			expected = "invalid Circle wallet transaction listing arguments"
		}
		requireCirclePayoutLedgerRefusal(t, defect, err, expected)
		if transactions != nil || calls != 0 || tokens != 0 || errors.Is(err, context.Canceled) != (defect == "canceled") {
			t.Fatal("invalid listing arguments reached Circle or the configuration", defect, err, calls, tokens)
		}
	}
}

// RC-13 The balance is the exact amount of the token whose address matches
// case-insensitively, on any page; zero when the token is absent.
func TestCirclePayoutLedgerBalanceMatchesTokenAddressOnEveryPage(t *testing.T) {
	for _, test := range []struct {
		name     string
		address  string
		amount   string
		expected model.NanoCents
	}{
		{"checksum-case", "0xAbCdEf0123456789AbCdEf0123456789AbCdEf01", "1234.567891", 1_234_567_891_000},
		{"exact-case", circlePayoutLedgerTestToken, "0.000001", 1_000},
		{"zero", circlePayoutLedgerTestToken, "0", 0},
		{"absent", "", "", 0},
	} {
		calls, closed, tokens := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
			requireCirclePayoutLedgerBalanceRequest(t, request)
			if closed != calls {
				t.Fatal("prior body was not closed")
			}
			calls++
			if request.URL.Query().Get("pageAfter") == "" {
				return circlePayoutLedgerTestResponse(t, request, "tokenBalances", []any{
					circlePayoutLedgerTestBalance("native-token", "", "5"),
					circlePayoutLedgerTestBalance("other-token", "0xabcdef0123456789abcdef0123456789abcdef02", "7.5"),
				}, circleReadTestNext(request, "opaque-balance-next"), &closed), nil
			}
			items := []any{}
			if test.address != "" {
				items = append(items, circlePayoutLedgerTestBalance("usdc-token", test.address, test.amount))
			}
			return circlePayoutLedgerTestResponse(t, request, "tokenBalances", items, "", &closed), nil
		}})
		client.readToken = func() string {
			tokens++
			return "synthetic-read-token"
		}
		balance, err := client.GetWalletTokenBalance(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestToken)
		if err != nil || balance != test.expected || calls != 2 || closed != calls || tokens != 1 {
			t.Fatal("token balance was not read exactly from every page", test.name, balance, err, calls, closed, tokens)
		}
	}
}

// RC-13 A partial, malformed, ambiguous or inexact balance listing is an
// error, never a zero balance.
func TestCirclePayoutLedgerBalanceRefusesMalformedListing(t *testing.T) {
	refusals := map[string]string{
		"not-found":        "Bad status: 404",
		"missing-link":     "tokenBalances page has no pagination Link",
		"malformed":        "invalid character",
		"missing-array":    "Circle GET is missing tokenBalances",
		"null-token":       "missing, repeated or excess token",
		"token-id":         "missing, repeated or excess token",
		"duplicate-token":  "missing, repeated or excess token",
		"amount":           "Circle balance is not a nonnegative decimal",
		"negative":         "Circle balance is not a nonnegative decimal",
		"missing-amount":   "Circle balance has an invalid decimal amount",
		"precision":        "exceeds exact supported precision/range",
		"overflow":         "exceeds exact supported precision/range",
		"ambiguous":        "Circle token balance is ambiguous",
		"cycle":            "tokenBalances pagination repeated a page",
		"lost-include-all": "pagination lost fixed filters",
		"tokens":           "missing, repeated or excess token",
		"pages":            "tokenBalances exceed 128 pages",
	}
	for _, defect := range []string{"not-found", "missing-link", "malformed", "missing-array", "null-token", "token-id", "duplicate-token", "amount", "negative", "missing-amount", "precision", "overflow", "ambiguous", "cycle", "lost-include-all", "tokens", "pages"} {
		calls, closed, waits, count := 0, 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") },
			do: func(request *http.Request) (*http.Response, error) {
				calls++
				usdc := circlePayoutLedgerTestBalance("usdc-token", circlePayoutLedgerTestToken, "1")
				other := circlePayoutLedgerTestBalance("other-token", "0xabcdef0123456789abcdef0123456789abcdef02", "1")
				items := []any{other, usdc}
				next := ""
				switch defect {
				case "not-found":
					return paymentReadTestResponse(http.StatusNotFound, `{"code":404,"message":"Not found."}`, &closed), nil
				case "missing-link":
					return paymentReadTestResponse(http.StatusOK, circleReadTestPage(t, "tokenBalances", items), &closed), nil
				case "malformed":
					return circlePayoutLedgerTestPayload(request, "synthetic-not-json", "", &closed), nil
				case "missing-array":
					return circlePayoutLedgerTestPayload(request, `{"data":{}}`, "", &closed), nil
				case "null-token":
					other["token"] = nil
				case "token-id":
					other["token"].(map[string]any)["id"] = ""
				case "duplicate-token":
					items = []any{usdc, usdc}
				case "amount":
					other["amount"] = "NaN"
				case "negative":
					other["amount"] = "-1"
				case "missing-amount":
					delete(other, "amount")
				case "precision":
					usdc["amount"] = "0.0000000001"
				case "overflow":
					usdc["amount"] = "9999999999999999999999"
				case "ambiguous":
					items = append(items, circlePayoutLedgerTestBalance("usdc-variant", strings.ToUpper(circlePayoutLedgerTestToken), "1"))
				case "cycle":
					items = []any{}
					next = circleReadTestNext(request, "opaque-next")
				case "lost-include-all":
					uri := *request.URL
					query := uri.Query()
					query.Del("includeAll")
					query.Set("pageAfter", "opaque-next")
					uri.RawQuery = query.Encode()
					next = "<" + uri.String() + ">; rel=\"next\""
				case "tokens":
					items = []any{}
					for i := 0; i < circleReadPageSize; i++ {
						count++
						items = append(items, circlePayoutLedgerTestBalance(fmt.Sprint("token-", count), fmt.Sprint("0xsynthetic", count), "1"))
					}
					next = circleReadTestNext(request, fmt.Sprint("opaque-", calls))
				case "pages":
					items = []any{}
					next = circleReadTestNext(request, fmt.Sprint("opaque-", calls))
				}
				return circlePayoutLedgerTestResponse(t, request, "tokenBalances", items, next, &closed), nil
			},
		})
		balance, err := client.GetWalletTokenBalance(context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestToken)
		expectedCalls := map[string]int{"cycle": 2, "tokens": maximumCircleTokens/circleReadPageSize + 1, "pages": maximumCircleObservationPages}[defect]
		if expectedCalls == 0 {
			expectedCalls = 1
		}
		requireCirclePayoutLedgerRefusal(t, defect, err, refusals[defect])
		if balance != 0 || calls != expectedCalls || closed != calls || waits != 0 {
			t.Fatal("malformed balance listing became a balance or retried", defect, balance, err, calls, closed, waits)
		}
	}
}

// RC-13 Invalid arguments are refused before the configuration is read or any
// GET is made.
func TestCirclePayoutLedgerBalanceRefusesInvalidArguments(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var missing context.Context
	for _, defect := range []string{"nil-context", "canceled", "empty-wallet", "wallet-path", "empty-token", "oversized-token"} {
		calls, tokens := 0, 0
		client := &CoreCircleApiClient{
			readHooks: paymentReadHooks{do: func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") }},
			readToken: func() string { tokens++; return "synthetic-read-token" },
		}
		ctx, wallet, token := context.Background(), circlePayoutLedgerTestWallet, circlePayoutLedgerTestToken
		switch defect {
		case "nil-context":
			ctx = missing
		case "canceled":
			ctx = canceled
		case "empty-wallet":
			wallet = ""
		case "wallet-path":
			wallet = "synthetic/wallet"
		case "empty-token":
			token = ""
		case "oversized-token":
			token = "0x" + strings.Repeat("a", 511)
		}
		balance, err := client.GetWalletTokenBalance(ctx, wallet, token)
		expected := map[string]string{"nil-context": "payment GET requires a context", "canceled": context.Canceled.Error()}[defect]
		if expected == "" {
			expected = "invalid Circle wallet balance arguments"
		}
		requireCirclePayoutLedgerRefusal(t, defect, err, expected)
		if balance != 0 || calls != 0 || tokens != 0 || errors.Is(err, context.Canceled) != (defect == "canceled") {
			t.Fatal("invalid balance arguments reached Circle or the configuration", defect, err, calls, tokens)
		}
	}
}

// RC-5 RC-13 The census reads Circle through the core client unless a test
// substitutes the ledger.
func TestCirclePayoutLedgerDefaultsToCoreClient(t *testing.T) {
	original := NewCirclePayoutLedger()
	defer SetCirclePayoutLedger(original)
	if _, ok := original.(*CoreCircleApiClient); !ok {
		t.Fatalf("default ledger is %T", original)
	}
	substitute := paymentReadTestCircle(paymentReadHooks{})
	SetCirclePayoutLedger(substitute)
	if NewCirclePayoutLedger() != CirclePayoutLedger(substitute) {
		t.Fatal("ledger substitution was ignored")
	}
}

func circlePayoutLedgerTestDeveloperWallet(id, blockchain, state, custody string) map[string]any {
	return map[string]any{"id": id, "address": "synthetic-address-" + id, "blockchain": blockchain, "state": state, "custodyType": custody, "walletSetId": "synthetic-wallet-set", "accountType": "EOA", "createDate": "2026-01-01T00:00:00Z", "updateDate": "2026-10-01T00:00:00Z"}
}

func circlePayoutLedgerTestWalletIds(wallets []CircleWallet) []string {
	ids := []string{}
	for _, wallet := range wallets {
		ids = append(ids, wallet.ID)
	}
	return ids
}

// RC-5 Every developer wallet is listed, on every blockchain and in every
// state, across pages whose next links omit or change the page size; a
// user-controlled wallet in the response is left out.
func TestCirclePayoutLedgerListsEveryDeveloperWallet(t *testing.T) {
	pages := map[string]struct {
		pageSize string
		items    []any
		next     string
	}{
		"": {"50", []any{
			circlePayoutLedgerTestDeveloperWallet("wallet-sol", "SOL", "LIVE", "DEVELOPER"),
			circlePayoutLedgerTestDeveloperWallet("wallet-user", "MATIC", "LIVE", "ENDUSER"),
			circlePayoutLedgerTestDeveloperWallet("wallet-matic", "MATIC", "FROZEN", "DEVELOPER"),
		}, "<https://api.circle.com/v1/w3s/wallets?pageAfter=opaque-second>; rel=\"next\""},
		"opaque-second": {"50", []any{circlePayoutLedgerTestDeveloperWallet("wallet-eth", "ETH", "LIVE", "DEVELOPER")}, "<https://api.circle.com/v1/w3s/wallets?pageAfter=opaque-third&pageSize=20>; rel=\"next\""},
		"opaque-third":  {"20", []any{circlePayoutLedgerTestDeveloperWallet("wallet-base", "BASE", "LIVE", "DEVELOPER")}, ""},
	}
	calls, closed, tokens := 0, 0, 0
	client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
		calls++
		page, ok := pages[request.URL.Query().Get("pageAfter")]
		if !ok {
			t.Fatal("cursor was not taken from the response Link", request.URL)
		}
		requireCirclePayoutLedgerRequest(t, request, "/v1/w3s/wallets", map[string]string{"pageSize": page.pageSize})
		return circlePayoutLedgerTestResponse(t, request, "wallets", page.items, page.next, &closed), nil
	}})
	client.readToken = func() string {
		tokens++
		return "synthetic-read-token"
	}
	wallets, err := client.ListDeveloperWallets(context.Background())
	if err != nil || !slices.Equal(circlePayoutLedgerTestWalletIds(wallets), []string{"wallet-sol", "wallet-matic", "wallet-eth", "wallet-base"}) || calls != 3 || closed != calls || tokens != 1 {
		t.Fatalf("developer wallets were not all listed: %v err=%v calls=%d closed=%d tokens=%d", circlePayoutLedgerTestWalletIds(wallets), err, calls, closed, tokens)
	}
	frozen := wallets[1]
	if frozen.State != "FROZEN" || frozen.Blockchain != "MATIC" || frozen.Address != "synthetic-address-wallet-matic" || frozen.CustodyType != "DEVELOPER" || !frozen.CreateDate.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("listed wallet lost its fields: %+v", frozen)
	}
}

// RC-5 One invalid, repeated or unclassified wallet fails the whole listing;
// the valid pages before it are discarded, never returned short.
func TestCirclePayoutLedgerDeveloperWalletsRefuseInvalidWallets(t *testing.T) {
	for _, defect := range []string{"missing-id", "invalid-id", "missing-address", "missing-blockchain", "missing-date", "missing-custody", "unknown-custody", "null", "duplicate"} {
		calls, closed, waits := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") },
			do: func(request *http.Request) (*http.Response, error) {
				calls++
				valid := circlePayoutLedgerTestDeveloperWallet("wallet-valid", "SOL", "LIVE", "DEVELOPER")
				if calls == 1 {
					return circlePayoutLedgerTestResponse(t, request, "wallets", []any{valid}, circleReadTestNext(request, "opaque-next"), &closed), nil
				}
				wallet := circlePayoutLedgerTestDeveloperWallet("wallet-defect", "SOL", "LIVE", "DEVELOPER")
				items := []any{wallet}
				switch defect {
				case "missing-id":
					delete(wallet, "id")
				case "invalid-id":
					wallet["id"] = "synthetic/wallet"
				case "missing-address":
					delete(wallet, "address")
				case "missing-blockchain":
					delete(wallet, "blockchain")
				case "missing-date":
					delete(wallet, "createDate")
				case "missing-custody":
					delete(wallet, "custodyType")
				case "unknown-custody":
					wallet["custodyType"] = "SYNTHETIC"
				case "null":
					items = []any{nil}
				case "duplicate":
					items = []any{valid}
				}
				return circlePayoutLedgerTestResponse(t, request, "wallets", items, "", &closed), nil
			},
		})
		wallets, err := client.ListDeveloperWallets(context.Background())
		id, ok := map[string]string{"missing-id": "", "null": "", "invalid-id": "synthetic/wallet", "duplicate": "wallet-valid"}[defect]
		if !ok {
			id = "wallet-defect"
		}
		requireCirclePayoutLedgerRefusal(t, defect, err, fmt.Sprintf("Circle wallet listing has an invalid, repeated or unclassified wallet %q", id))
		if wallets != nil || calls != 2 || closed != calls || waits != 0 {
			t.Fatal("invalid listed wallet was accepted, skipped or retried", defect, err, calls, closed, waits)
		}
	}
}

// RC-5 A refused GET, a malformed page, a page without the documented Link,
// or a next link that adds a filter, holds both cursors or cycles fails the
// listing without a retry.
func TestCirclePayoutLedgerDeveloperWalletsRefuseHttpJsonAndPagination(t *testing.T) {
	refusals := map[string]string{
		"unauthorized":     "Bad status: 401",
		"malformed":        "invalid character",
		"missing-array":    "Circle GET is missing wallets",
		"object":           "Circle GET wallets is not an array",
		"oversized":        "Circle GET wallets exceeds requested page bound",
		"unparseable-date": "parsing time",
		"missing-link":     "wallets page has no pagination Link",
		"foreign-filter":   "pagination changed fixed filters",
		"both-cursors":     "pagination requires one opaque cursor",
		"cycle":            "wallets pagination repeated a page",
	}
	for _, defect := range []string{"unauthorized", "malformed", "missing-array", "object", "oversized", "unparseable-date", "missing-link", "foreign-filter", "both-cursors", "cycle"} {
		calls, closed, waits := 0, 0, 0
		client := paymentReadTestCircle(paymentReadHooks{
			wait: func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") },
			do: func(request *http.Request) (*http.Response, error) {
				calls++
				items := []any{circlePayoutLedgerTestDeveloperWallet(fmt.Sprint("wallet-", calls), "SOL", "LIVE", "DEVELOPER")}
				next := ""
				switch defect {
				case "unauthorized":
					return paymentReadTestResponse(http.StatusUnauthorized, `{"code":401,"message":"synthetic unauthorized"}`, &closed), nil
				case "malformed":
					return circlePayoutLedgerTestPayload(request, "synthetic-not-json", "", &closed), nil
				case "missing-array":
					return circlePayoutLedgerTestPayload(request, `{"data":{}}`, "", &closed), nil
				case "object":
					return circlePayoutLedgerTestPayload(request, `{"data":{"wallets":{}}}`, "", &closed), nil
				case "missing-link":
					return paymentReadTestResponse(http.StatusOK, circleReadTestPage(t, "wallets", items), &closed), nil
				case "oversized":
					for i := 0; i < circleReadPageSize; i++ {
						items = append(items, circlePayoutLedgerTestDeveloperWallet(fmt.Sprint("wallet-extra-", i), "SOL", "LIVE", "DEVELOPER"))
					}
				case "unparseable-date":
					items[0].(map[string]any)["createDate"] = "2026-01-01 00:00:00"
				case "foreign-filter":
					next = "<https://api.circle.com/v1/w3s/wallets?blockchain=SOL&pageAfter=opaque-next&pageSize=50>; rel=\"next\""
				case "both-cursors":
					next = "<https://api.circle.com/v1/w3s/wallets?pageAfter=opaque-next&pageBefore=opaque-previous>; rel=\"next\""
				case "cycle":
					next = "<https://api.circle.com/v1/w3s/wallets?pageAfter=opaque-next>; rel=\"next\""
				}
				return circlePayoutLedgerTestResponse(t, request, "wallets", items, next, &closed), nil
			},
		})
		wallets, err := client.ListDeveloperWallets(context.Background())
		expectedCalls := 1
		if defect == "cycle" {
			expectedCalls = 2
		}
		requireCirclePayoutLedgerRefusal(t, defect, err, refusals[defect])
		if wallets != nil || calls != expectedCalls || closed != calls || waits != 0 {
			t.Fatal("refused wallet listing was accepted or retried", defect, err, calls, closed, waits)
		}
	}
}

// RC-5 More wallet pages than the bound is an error, never a truncated listing.
func TestCirclePayoutLedgerDeveloperWalletPageBoundIsAnError(t *testing.T) {
	calls, closed := 0, 0
	client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
		calls++
		next := fmt.Sprintf("<https://api.circle.com/v1/w3s/wallets?pageAfter=opaque-%d&pageSize=50>; rel=\"next\"", calls)
		return circlePayoutLedgerTestPayload(request, `{"data":{"wallets":[]}}`, next, &closed), nil
	}})
	wallets, err := client.ListDeveloperWallets(context.Background())
	requireCirclePayoutLedgerRefusal(t, "page-bound", err, "wallets exceed 50000 pages")
	if wallets != nil || calls != maximumCirclePayoutLedgerPages || closed != calls {
		t.Fatal("wallet listing beyond the page bound looked complete", err, calls, closed)
	}
}

// RC-5 A missing or canceled context is refused before the configuration is
// read or any GET is made.
func TestCirclePayoutLedgerDeveloperWalletsRefuseDeadContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var missing context.Context
	for _, defect := range []string{"nil-context", "canceled"} {
		calls, tokens := 0, 0
		client := &CoreCircleApiClient{
			readHooks: paymentReadHooks{do: func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected request") }},
			readToken: func() string { tokens++; return "synthetic-read-token" },
		}
		ctx, expected := missing, "wallets listing requires a context"
		if defect == "canceled" {
			ctx, expected = canceled, context.Canceled.Error()
		}
		wallets, err := client.ListDeveloperWallets(ctx)
		requireCirclePayoutLedgerRefusal(t, defect, err, expected)
		if wallets != nil || calls != 0 || tokens != 0 {
			t.Fatal("dead context reached Circle or the configuration", defect, err, calls, tokens)
		}
	}
}
