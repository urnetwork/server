// Exercise the real bounded proof RPC envelope independently of trie/VM
// completeness. Only the separately selected SDK/Go joins prove those paths.
package strecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeExecutionProofReaderRetainsParentChildAndRetryBudget(t *testing.T) {
	reader, err := NewNativeExecutionProofReader("http://native.example", 300*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	parent := "0x" + strings.Repeat("17", 32)
	child := []byte(":child_storage:default:original")
	calls, waits := 0, 0
	reader.rpc.wait = func(ctx context.Context, _ time.Duration) error { waits++; return ctx.Err() }
	reader.rpc.client.Transport = nativeReadTestTransport(func(request *http.Request) (*http.Response, error) {
		var call struct {
			Id     int               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
			t.Fatal(err)
		}
		calls++
		if call.Method != "state_getChildReadProof" || len(call.Params) != 3 || string(call.Params[2]) != fmt.Sprintf("%q", parent) {
			t.Fatal("child proof route/parent changed", call)
		}
		result := "null"
		if calls == 2 {
			result = fmt.Sprintf(`{"at":%q,"proof":["0x01"]}`, parent)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &nativeReadTestBody{raw: []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, call.Id, result))}}, nil
	})
	proof, err := reader.Read(t.Context(), parent, child, []byte{0xa0})
	if err != nil || proof == nil || proof.At != parent || calls != 2 || waits != 1 || reader.rpc.requests != 2 {
		t.Fatal("missing exact proof reset its budget or became absence", proof, err, calls, waits)
	}
}

func TestNativeExecutionProofReaderConflictDominatesTransientTail(t *testing.T) {
	for _, fault := range []string{"identity", "parent", "malformed", "permanent"} {
		reader, err := NewNativeExecutionProofReader("http://native.example", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		parent := "0x" + strings.Repeat("17", 32)
		waits := 0
		reader.rpc.wait = func(context.Context, time.Duration) error { waits++; return errors.New("unexpected retry") }
		reader.rpc.client.Transport = nativeReadTestTransport(func(*http.Request) (*http.Response, error) {
			raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"at":%q,"proof":["0x01"]}}`, parent)
			var tail error
			switch fault {
			case "identity":
				raw = strings.Replace(raw, `"id":1`, `"id":2`, 1)
				tail = syscall.EIO
			case "parent":
				raw = strings.Replace(raw, parent, "0x"+strings.Repeat("18", 32), 1)
				tail = syscall.EIO
			case "malformed":
				raw = `{"jsonrpc":"2.0","id":1,"result":7}`
			case "permanent":
				tail = errors.Join(syscall.EIO, errors.New("permanent proof transport"))
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &nativeReadTestBody{raw: []byte(raw), tail: tail}}, nil
		})
		proof, err := reader.Read(t.Context(), parent, nil, nil)
		reader.Close()
		if err == nil || proof != nil || waits != 0 {
			t.Fatal("contradictory or permanent proof cause retried", fault, err, waits)
		}
		if fault != "permanent" && !errors.Is(err, ErrNativeExecutionProofConflict) {
			t.Fatal("complete proof conflict lost classification", fault, err)
		}
		if fault == "identity" && !errors.Is(err, syscall.EIO) {
			t.Fatal("conflict dropped adjacent read cause", err)
		}
	}
}
