// The platform-local resolver of a peer's identity key.
package capprobe

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The resolver answers what the platform's store holds for the peer: its
// published key, no key and no error for a peer that never published one,
// and an error, never a panic, for a read that fails or raises.
func TestLocalPeerKeyFetcherResolvesThroughThePlatformStore(t *testing.T) {
	published, unpublished, failing, raising := connect.NewId(), connect.NewId(), connect.NewId(), connect.NewId()
	key := bytes.Repeat([]byte{7}, 32)
	readErr := errors.New("synthetic key read failure")
	previous := getClientPublicKey
	getClientPublicKey = func(ctx context.Context, clientId server.Id) ([]byte, error) {
		switch connect.Id(clientId) {
		case published:
			return key, nil
		case failing:
			return nil, readErr
		case raising:
			panic(errors.New("synthetic raised datastore failure"))
		default:
			return nil, nil
		}
	}
	t.Cleanup(func() { getClientPublicKey = previous })

	if fetched, err := newLocalPeerClientPublicKeyFetcher(published)(context.Background()); err != nil || !bytes.Equal(fetched, key) {
		t.Fatalf("fetched %x, %v; want the published key", fetched, err)
	}
	if fetched, err := newLocalPeerClientPublicKeyFetcher(unpublished)(context.Background()); err != nil || len(fetched) != 0 {
		t.Fatalf("an unpublished peer fetched %x, %v", fetched, err)
	}
	if _, err := newLocalPeerClientPublicKeyFetcher(failing)(context.Background()); !errors.Is(err, readErr) {
		t.Fatalf("a failed read returned %v", err)
	}
	if fetched, err := newLocalPeerClientPublicKeyFetcher(raising)(context.Background()); err == nil || len(fetched) != 0 {
		t.Fatalf("a raised read returned %x, %v", fetched, err)
	}
}
