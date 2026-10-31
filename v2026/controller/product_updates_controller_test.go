package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const brevoProtocolFixtureApiKey = "fixture-brevo-api-key"

// Pins both the endpoint and credentials to one fixture; protocol tests must
// never resolve an operator's vault, even when an API key was already cached.
func newBrevoProtocolTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	t.Setenv("WARP_VAULT_HOME", t.TempDir())
	previousBaseUrl := brevoApiBaseUrl
	previousApiKey := brevoApiKey
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if values := r.Header.Values("api-key"); len(values) != 1 || values[0] != brevoProtocolFixtureApiKey {
			t.Error("protocol request did not use exactly one fixture API key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	brevoApiBaseUrl = testServer.URL + "/v3"
	brevoApiKey = func() string { return brevoProtocolFixtureApiKey }
	t.Cleanup(func() {
		testServer.Close()
		brevoApiBaseUrl = previousBaseUrl
		brevoApiKey = previousApiKey
	})
	return testServer
}

func requireBrevoProtocolError(t *testing.T, err error, expected string, forbidden ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a bounded Brevo protocol error")
	}
	actual := err.Error()
	if actual != expected {
		t.Fatalf("Brevo protocol error format mismatch (bytes=%d)", len(actual))
	}
	for i, value := range forbidden {
		if strings.Contains(actual, value) {
			t.Fatalf("Brevo protocol error retained forbidden fixture field %d", i)
		}
	}
}

func TestBrevo(t *testing.T) {
	contacts := map[string]bool{}
	listEmails := map[string]map[string]bool{}

	writeJson := func(w http.ResponseWriter, statusCode int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
	newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v3/contacts":
			var args BrevoContactArgs
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
				writeJson(w, http.StatusBadRequest, BrevoContactResult{Code: "invalid_json"})
				return
			}
			if contacts[args.Email] {
				writeJson(w, http.StatusBadRequest, BrevoContactResult{Code: "duplicate_parameter"})
				return
			}
			contacts[args.Email] = true
			writeJson(w, http.StatusCreated, BrevoContactResult{Id: 1})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v3/contacts/"):
			userEmail := strings.TrimPrefix(r.URL.Path, "/v3/contacts/")
			if !contacts[userEmail] {
				writeJson(w, http.StatusNotFound, BrevoContactResult{Code: "document_not_found"})
				return
			}
			delete(contacts, userEmail)
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v3/contacts/lists/"):
			var args BrevoListArgs
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil || len(args.Emails) != 1 {
				writeJson(w, http.StatusBadRequest, BrevoListResult{Code: "invalid_json"})
				return
			}
			userEmail := args.Emails[0]
			pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v3/contacts/lists/"), "/")
			if len(pathParts) != 3 || pathParts[1] != "contacts" {
				writeJson(w, http.StatusNotFound, BrevoListResult{Code: "not_found"})
				return
			}
			listId := pathParts[0]
			emails := listEmails[listId]
			if emails == nil {
				emails = map[string]bool{}
				listEmails[listId] = emails
			}
			switch pathParts[2] {
			case "add":
				if emails[userEmail] || !contacts[userEmail] {
					writeJson(w, http.StatusBadRequest, BrevoListResult{Code: "invalid_parameter"})
					return
				}
				emails[userEmail] = true
			case "remove":
				if !emails[userEmail] {
					writeJson(w, http.StatusBadRequest, BrevoListResult{Code: "invalid_parameter"})
					return
				}
				delete(emails, userEmail)
			default:
				writeJson(w, http.StatusNotFound, BrevoListResult{Code: "not_found"})
				return
			}
			writeJson(w, http.StatusOK, BrevoListResult{
				Contacts: &BrevoListResultContacts{
					Success: []string{userEmail},
				},
			})

		default:
			writeJson(w, http.StatusNotFound, BrevoContactResult{Code: "not_found"})
		}
	}))

	ctx := context.Background()
	userEmails := []string{}
	for range 4 {
		userEmails = append(userEmails, fmt.Sprintf("member.%d@example.invalid", len(userEmails)))
	}
	listIds := []int{11, 12}

	for _, userEmail := range userEmails {
		if err := BrevoAddContact(ctx, userEmail); err != nil {
			t.Fatalf("add contact %s: %v", userEmail, err)
		}
		if err := BrevoAddContact(ctx, userEmail); err != nil {
			t.Fatalf("add duplicate contact %s: %v", userEmail, err)
		}
		for _, listId := range listIds {
			if err := BrevoAddToList(ctx, userEmail, listId); err != nil {
				t.Fatalf("add %s to list %d: %v", userEmail, listId, err)
			}
			if err := BrevoAddToList(ctx, userEmail, listId); err != nil {
				t.Fatalf("add duplicate %s to list %d: %v", userEmail, listId, err)
			}
			if err := BrevoRemoveFromList(ctx, userEmail, listId); err != nil {
				t.Fatalf("remove %s from list %d: %v", userEmail, listId, err)
			}
			if err := BrevoRemoveFromList(ctx, userEmail, listId); err != nil {
				t.Fatalf("remove duplicate %s from list %d: %v", userEmail, listId, err)
			}
		}
		if err := BrevoRemoveContact(ctx, userEmail); err != nil {
			t.Fatalf("remove contact %s: %v", userEmail, err)
		}
		if err := BrevoRemoveContact(ctx, userEmail); err != nil {
			t.Fatalf("remove duplicate contact %s: %v", userEmail, err)
		}
	}
}

func TestBrevoAddContactRejectsMalformedSuccessResponse(t *testing.T) {
	newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		if _, err := w.Write([]byte("{")); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))

	if err := BrevoAddContact(context.Background(), "member@example.invalid"); err == nil {
		t.Fatal("malformed successful response was accepted")
	}
}

func TestBrevoAddToListRejectsMissingContactsResult(t *testing.T) {
	requestCount := 0
	newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			w.WriteHeader(http.StatusCreated)
			if _, err := w.Write([]byte(`{"id":1}`)); err != nil {
				t.Errorf("write contact response: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{}`)); err != nil {
			t.Errorf("write list response: %v", err)
		}
	}))

	if err := BrevoAddToList(context.Background(), "member@example.invalid", 11); err == nil {
		t.Fatal("success response without contacts was accepted")
	}
}

func TestBrevoAddContactReportsOnlySafeProviderCode(t *testing.T) {
	const (
		userEmail       = "member@example.invalid"
		providerMessage = "contact member@example.invalid uses fixture-private-value at service.example.invalid"
	)
	newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if err := json.NewEncoder(w).Encode(BrevoContactResult{
			Code:    "invalid_parameter",
			Message: providerMessage,
		}); err != nil {
			t.Error("encode fixture response")
		}
	}))

	err := BrevoAddContact(context.Background(), userEmail)
	requireBrevoProtocolError(
		t,
		err,
		"Brevo add-contact failed (status=400 Bad Request; provider_code=invalid_parameter; failure_class=provider-rejection)",
		userEmail,
		providerMessage,
		"fixture-private-value",
		"service.example.invalid",
		brevoProtocolFixtureApiKey,
	)
}

func TestBrevoAddContactRedactsUntrustedProviderFields(t *testing.T) {
	const (
		userEmail       = "member@example.invalid"
		providerMessage = "fixture-private-value for member@example.invalid at service.example.invalid"
	)
	tests := []struct {
		name         string
		responseBody string
		failureClass string
	}{
		{
			name: "hostile code",
			responseBody: `{"code":"invalid_parameter\nmember@example.invalid fixture-private-value",` +
				`"message":"` + providerMessage + `"}`,
			failureClass: "provider-rejection",
		},
		{
			name: "malformed code",
			responseBody: `{"code":{"value":"member@example.invalid"},` +
				`"message":"` + providerMessage + `"}`,
			failureClass: "response-decode",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				if _, err := w.Write([]byte(test.responseBody)); err != nil {
					t.Error("write fixture response")
				}
			}))

			err := BrevoAddContact(context.Background(), userEmail)
			requireBrevoProtocolError(
				t,
				err,
				fmt.Sprintf(
					"Brevo add-contact failed (status=400 Bad Request; provider_code=unclassified; failure_class=%s)",
					test.failureClass,
				),
				userEmail,
				providerMessage,
				"fixture-private-value",
				"service.example.invalid",
				brevoProtocolFixtureApiKey,
			)
		})
	}
}

func TestBrevoAddContactRedactsTransportURL(t *testing.T) {
	const userEmail = "member@example.invalid"
	testServer := newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("closed fixture server received a request")
	}))
	fixtureUrl := testServer.URL
	testServer.Close()

	err := BrevoAddContact(context.Background(), userEmail)
	requireBrevoProtocolError(
		t,
		err,
		"Brevo add-contact failed (status=unavailable; provider_code=unclassified; failure_class=transport)",
		userEmail,
		fixtureUrl,
		brevoProtocolFixtureApiKey,
	)
}

func TestBrevoProductUpdateOperationsUseBoundedErrors(t *testing.T) {
	const (
		userEmail       = "member@example.invalid"
		listId          = 41
		providerCode    = "invalid_json"
		providerMessage = "fixture-private-value for member@example.invalid at service.example.invalid"
	)
	tests := []struct {
		name             string
		operation        brevoProductUpdatesOperation
		method           string
		path             string
		bootstrapContact bool
		request          func(context.Context) error
	}{
		{
			name:      "add contact",
			operation: brevoOperationAddContact,
			method:    http.MethodPost,
			path:      "/v3/contacts",
			request: func(ctx context.Context) error {
				return BrevoAddContact(ctx, userEmail)
			},
		},
		{
			name:      "remove contact",
			operation: brevoOperationRemoveContact,
			method:    http.MethodDelete,
			path:      "/v3/contacts/" + userEmail,
			request: func(ctx context.Context) error {
				return BrevoRemoveContact(ctx, userEmail)
			},
		},
		{
			name:             "add to list",
			operation:        brevoOperationAddToList,
			method:           http.MethodPost,
			path:             fmt.Sprintf("/v3/contacts/lists/%d/contacts/add", listId),
			bootstrapContact: true,
			request: func(ctx context.Context) error {
				return BrevoAddToList(ctx, userEmail, listId)
			},
		},
		{
			name:      "remove from list",
			operation: brevoOperationRemoveFromList,
			method:    http.MethodPost,
			path:      fmt.Sprintf("/v3/contacts/lists/%d/contacts/remove", listId),
			request: func(ctx context.Context) error {
				return BrevoRemoveFromList(ctx, userEmail, listId)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bootstrapObserved := false
			rejectionObserved := false
			newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case test.bootstrapContact && r.Method == http.MethodPost && r.URL.Path == "/v3/contacts":
					bootstrapObserved = true
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					if _, err := w.Write([]byte(`{"id":1}`)); err != nil {
						t.Error("write contact bootstrap response")
					}
				case r.Method == test.method && r.URL.Path == test.path:
					rejectionObserved = true
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					if err := json.NewEncoder(w).Encode(BrevoContactResult{
						Code:    providerCode,
						Message: providerMessage,
					}); err != nil {
						t.Error("encode rejected response")
					}
				default:
					t.Error("unexpected product-updates request")
					w.WriteHeader(http.StatusNotFound)
				}
			}))

			err := test.request(context.Background())
			requireBrevoProtocolError(
				t,
				err,
				fmt.Sprintf(
					"Brevo %s failed (status=400 Bad Request; provider_code=%s; failure_class=provider-rejection)",
					test.operation,
					providerCode,
				),
				userEmail,
				providerMessage,
				"fixture-private-value",
				"service.example.invalid",
				brevoProtocolFixtureApiKey,
			)
			if test.bootstrapContact != bootstrapObserved {
				t.Fatal("unexpected contact-bootstrap request state")
			}
			if !rejectionObserved {
				t.Fatal("rejected operation request was not observed")
			}
		})
	}
}

func TestMaskEmailWithoutHostDoesNotPanic(t *testing.T) {
	if masked := maskEmail("not-an-email"); masked == "" {
		t.Fatal("maskEmail returned an empty value")
	}
}
