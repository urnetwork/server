package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderEgressLocationSubmitRejectsMissingSecret(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"client_id": "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-location", bytes.NewReader(body))
	w := httptest.NewRecorder()

	ProviderEgressLocationSubmit(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when the operator secret header is absent", w.Code)
	}
}

func TestProviderEgressLocationSubmitRejectsWrongSecret(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"client_id": "019f8835-158d-6fd8-e9dd-fd0e4c6d6792",
	})
	req := httptest.NewRequest(http.MethodPost, "/network/provider-egress-location", bytes.NewReader(body))
	req.Header.Set(operatorSecretHeader, "definitely-not-the-secret")
	w := httptest.NewRecorder()

	ProviderEgressLocationSubmit(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 on a wrong operator secret", w.Code)
	}
}
