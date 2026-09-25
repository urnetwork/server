package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/proxy/flowtrace"
)

func TestFlowTraceAPIIsScopedAndReadsNeverCreateDevices(t *testing.T) {
	owner := server.Id{1}
	other := server.Id{2}
	device := &ProxyDevice{}
	opens := 0
	handler := flowTraceHandler{
		auth: func(token string) (server.Id, error) {
			switch token {
			case "owner":
				return owner, nil
			case "other":
				return other, nil
			}
			return server.Id{}, errors.New("rejected")
		},
		open: func(id server.Id) (*ProxyDevice, error) {
			opens++
			if id != owner {
				return nil, errors.New("missing")
			}
			return device, nil
		},
		lookup: func(id server.Id) *ProxyDevice {
			if id == owner {
				return device
			}
			return nil
		},
	}
	call := func(method, token, session string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, flowTracePath+"?cursor_only=1", strings.NewReader(`{"port":443}`))
		request.Header.Set("Authorization", token)
		request.Header.Set("X-UR-Flow-Trace", session)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := call(http.MethodPost, "bad", ""); response.Code != http.StatusUnauthorized || opens != 0 {
		t.Fatalf("unauthenticated activation code=%d opens=%d", response.Code, opens)
	}
	if response := call(http.MethodGet, "owner", ""); response.Code != http.StatusGone || opens != 0 {
		t.Fatalf("unarmed read code=%d opens=%d", response.Code, opens)
	}
	response := call(http.MethodPost, "owner", "")
	var snapshot flowtrace.Snapshot
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil || snapshot.Session == "" {
		t.Fatalf("activation failed: %d %s", response.Code, response.Body.String())
	}
	for _, test := range []struct {
		token, session string
		status         int
	}{
		{"owner", snapshot.Session, http.StatusOK},
		{"other", snapshot.Session, http.StatusGone},
		{"owner", "wrong-generation", http.StatusGone},
	} {
		if response := call(http.MethodGet, test.token, test.session); response.Code != test.status {
			t.Errorf("read status=%d want=%d", response.Code, test.status)
		}
	}
	if opens != 1 {
		t.Fatalf("reads created devices: opens=%d", opens)
	}
	expired, err := flowtrace.New(443, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	device.flowTrace.Store(expired)
	if response := call(http.MethodGet, "owner", expired.Cursor(time.Now()).Session); response.Code != http.StatusGone {
		t.Fatalf("expired trace remained readable: %d", response.Code)
	}
}
