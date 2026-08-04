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
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer testServer.Close()

	previousBaseUrl := brevoApiBaseUrl
	brevoApiBaseUrl = testServer.URL + "/v3"
	defer func() {
		brevoApiBaseUrl = previousBaseUrl
	}()

	ctx := context.Background()
	userEmails := []string{}
	for range 4 {
		userEmails = append(userEmails, fmt.Sprintf("test.%d@ur.io", len(userEmails)))
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
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		if _, err := w.Write([]byte("{")); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer testServer.Close()

	previousBaseUrl := brevoApiBaseUrl
	brevoApiBaseUrl = testServer.URL
	defer func() {
		brevoApiBaseUrl = previousBaseUrl
	}()

	if err := BrevoAddContact(context.Background(), "test@ur.io"); err == nil {
		t.Fatal("malformed successful response was accepted")
	}
}

func TestBrevoAddToListRejectsMissingContactsResult(t *testing.T) {
	requestCount := 0
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	defer testServer.Close()

	previousBaseUrl := brevoApiBaseUrl
	brevoApiBaseUrl = testServer.URL
	defer func() {
		brevoApiBaseUrl = previousBaseUrl
	}()

	if err := BrevoAddToList(context.Background(), "test@ur.io", 11); err == nil {
		t.Fatal("success response without contacts was accepted")
	}
}

func TestMaskEmailWithoutHostDoesNotPanic(t *testing.T) {
	if masked := maskEmail("not-an-email"); masked == "" {
		t.Fatal("maskEmail returned an empty value")
	}
}
