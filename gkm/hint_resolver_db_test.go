//go:build sidecar_pending

package gkm

// gkm's hint addressing end to end on the private stack (plan-KEYS-hint-addressing, QD-146, QD-1171): a hint gkm's
// writer sends by sidecar id lands on the holder's current client, the newest binding generation's, and on no other
// client, its older generation's, another sidecar's and a revoked sidecar's included, through api's internal route
// over MG1's tables and the hint script on the test Redis; the route's resolver is what server/cli/gkm builds.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// 32 random bytes.
func hintResolverTestRandom(t testing.TB) []byte {
	t.Helper()
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return value
}

// Binds sidecarId's generation to a new client in networkId under identityKey, with synthetic binding bytes (the
// model checks no signature), and answers the client.
func hintResolverTestBind(t testing.TB, ctx context.Context, networkId server.Id, sidecarId server.Id, identityKey []byte, generation int64) server.Id {
	t.Helper()
	clientId := server.NewId()
	outcome, _ := model.BindSidecarIdentity(ctx, &model.SidecarBindingRecord{
		NetworkId:         networkId,
		SidecarId:         sidecarId,
		ClientId:          clientId,
		Generation:        generation,
		ClientPublicKey:   hintResolverTestRandom(t),
		IdentityPublicKey: identityKey,
		SignedBinding:     append([]byte("synthetic binding of "), clientId.Bytes()...),
	}, server.NowUtc())
	if outcome != model.SidecarBindAppended {
		t.Fatalf("bind: %s", outcome)
	}
	return clientId
}

func TestGkmHintBySidecarLandsOnItsCurrentClientAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		holder := server.NewId()
		holderKey := hintResolverTestRandom(t)
		holderOldClient := hintResolverTestBind(t, ctx, networkId, holder, holderKey, 1)
		holderClient := hintResolverTestBind(t, ctx, networkId, holder, holderKey, 2)
		joiner := server.NewId()
		joinerClient := hintResolverTestBind(t, ctx, networkId, joiner, hintResolverTestRandom(t), 1)
		revoked := server.NewId()
		revokedClient := hintResolverTestBind(t, ctx, networkId, revoked, hintResolverTestRandom(t), 1)
		if !model.SetSidecarIdentityRevoked(ctx, revoked, server.NowUtc()) {
			t.Fatal("not revoked")
		}

		// api's route on a verify file holding gkm's bearer, over MG1's tables
		raw := make([]byte, server.DefaultInternalGateSettings().BearerByteCount)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		bearer := base64.RawURLEncoding.EncodeToString(raw)
		digest := sha256.Sum256([]byte(bearer))
		verifier, err := server.NewInternalGateVerifier([]byte("gkm: {class: gkm, sha256: [" + hex.EncodeToString(digest[:]) + "]}\n"))
		if err != nil {
			t.Fatal(err)
		}
		routerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		api := httptest.NewServer(router.NewRouter(routerCtx, handlers.NewInternalGateIdentityClientsHandlers(&handlers.InternalGateIdentityClientsDeps{Verifier: verifier}).Routes()))
		defer api.Close()

		resolver, err := server.NewSidecarIdentityClientsResolver(&server.SidecarIdentityClientsResolverArgs{ApiUrl: api.URL, Bearer: bearer, HttpClient: api.Client()})
		if err != nil {
			t.Fatal(err)
		}
		redis := model.NewSidecarPushRedis(&model.SidecarPushRedisSettings{CallTimeout: 10 * time.Second})
		writer, err := pushservice.NewHintWriter(redis, nil, resolver, nil)
		if err != nil {
			t.Fatal(err)
		}

		// a late ask's wrap-pending naming the joiner, to the holder and to the revoked sidecar
		body, err := proto.Marshal(&protocol.GkmHint{
			GroupId:   sidecar.NewId().Bytes(),
			Kind:      protocol.GkmHintKind_GKM_HINT_WRAP_PENDING,
			Recipient: []byte("s:" + sidecar.EvsKeyId(sidecar.Id(joiner))),
		})
		if err != nil {
			t.Fatal(err)
		}
		hint := &sidecar.ClientHint{
			Topic:   sidecar.PushTopicGkm,
			Version: uint64(time.Now().UnixMicro()),
			Kind:    uint32(protocol.MessageType_GKM_HINT),
			Body:    body,
		}
		if err := writer.HintSidecars(ctx, []connect.Id{connect.Id(holder), connect.Id(revoked)}, hint); err != nil {
			t.Fatal(err)
		}
		if stats := writer.Stats(); stats.HintedCount != 1 || stats.UnresolvedCount != 1 {
			t.Fatalf("hinted %d, unresolved %d: want the holder hinted and the revoked sidecar unresolved", stats.HintedCount, stats.UnresolvedCount)
		}

		entry, found, err := pushservice.ReadHintTail(ctx, redis, connect.Id(holderClient))
		if err != nil || !found {
			t.Fatalf("the holder's current client holds no hint: %v", err)
		}
		landed, err := pushservice.ParseHintEntry(entry)
		if err != nil {
			t.Fatal(err)
		}
		if landed.GetTopic() != sidecar.PushTopicGkm || landed.GetKind() != uint32(protocol.MessageType_GKM_HINT) || !proto.Equal(mustGkmHint(t, landed.GetHint()), mustGkmHint(t, body)) {
			t.Fatalf("the holder's hint: %+v", landed)
		}
		for name, clientId := range map[string]server.Id{
			"the holder's older generation": holderOldClient,
			"the joiner":                    joinerClient,
			"the revoked sidecar":           revokedClient,
		} {
			if _, found, err := pushservice.ReadHintTail(ctx, redis, connect.Id(clientId)); err != nil || found {
				t.Errorf("%s's client holds a hint: %v", name, err)
			}
		}
	})
}

// A GkmHint's bytes, parsed.
func mustGkmHint(t testing.TB, body []byte) *protocol.GkmHint {
	t.Helper()
	hint := &protocol.GkmHint{}
	if err := proto.Unmarshal(body, hint); err != nil {
		t.Fatal(err)
	}
	return hint
}
