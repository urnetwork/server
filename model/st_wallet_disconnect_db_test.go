package model

// The payout wallet disconnect (st_wallet_disconnect.go, support inbox 1099)
// against the test env. Before it, a connected wallet could only be replaced:
// no path removed the network's side copy or a provider's account-history
// wallet, so a wallet set once was paid, and shown, for good. Each test drives
// the model's code and disconnect functions and reads back what settlement and
// the apps read.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urfoundation/sn/ss58"

	"github.com/urnetwork/server"
)

// A fresh Bittensor coldkey: its key and ss58 address.
func stDisconnectTestColdkey(t testing.TB) ([32]byte, string) {
	t.Helper()
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	address, err := ss58.Encode(key, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	return key, address
}

// A network with a verified email sign-in, its admin, and its address.
func stDisconnectTestNetwork(t testing.TB, ctx context.Context) (networkId server.Id, userId server.Id, userAuth string) {
	t.Helper()
	networkId = server.NewId()
	userId = server.NewId()
	userAuth = Testing_CreateNetwork(ctx, networkId, "disconnect-"+networkId.String(), userId)
	return
}

// Whether a payout wallet notice owed to the network lists the removal of the
// wallet at the destination: its previous wallet masked, and none after.
func stDisconnectTestNoticeListsRemoval(t testing.TB, ctx context.Context, networkId server.Id, destination string, address string) bool {
	t.Helper()
	notices := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT template_json::text FROM account_message_outbox WHERE network_id = $1 AND template_name = $2`, networkId, SecurityPayoutWalletTemplateName)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var notice string
				server.Raise(result.Scan(&notice))
				notices = append(notices, notice)
			}
		})
	})
	for _, raw := range notices {
		notice := &SecurityWalletNotice{}
		if err := json.Unmarshal([]byte(raw), notice); err != nil {
			t.Fatal(err)
		}
		for _, change := range notice.Changes {
			if change.Destination == destination && change.PreviousWallet == MaskWalletAddress(address) && change.NewWallet == "" {
				return true
			}
		}
	}
	return false
}

// The root cause: a disconnect removes the network's side copy and ends each
// provider's account-history wallet, so neither the apps nor settlement see a
// wallet afterwards, and a provider of another network is untouched. The code
// confirms one disconnect only, and a wallet connected afterwards pays again.
func TestStWalletDisconnectRemovesTheConnectedWallets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultStWalletDisconnectSettings()
		networkId, userId, userAuth := stDisconnectTestNetwork(t, ctx)
		otherNetworkId, _, _ := stDisconnectTestNetwork(t, ctx)

		networkKey, networkAddress := stDisconnectTestColdkey(t)
		SetStWalletOriginal(ctx, networkId, nil, networkAddress, networkKey, nil, nil)
		clientId := server.NewId()
		providerKey, providerAddress := stDisconnectTestColdkey(t)
		SetStWalletOriginal(ctx, networkId, &clientId, providerAddress, providerKey, nil, nil)
		otherClientId := server.NewId()
		otherKey, otherAddress := stDisconnectTestColdkey(t)
		SetStWalletOriginal(ctx, otherNetworkId, &otherClientId, otherAddress, otherKey, nil, nil)

		if !HasDisconnectableStWallet(ctx, networkId) {
			t.Fatal("the connected wallets are not disconnectable")
		}

		code, err := SendStWalletDisconnectCode(ctx, settings, userId, networkId)
		if err != nil {
			t.Fatal(err)
		}
		if len(code.Recipients) != 1 || code.Recipients[0].Address != userAuth || code.Recipients[0].Masked != maskEmailAddress(userAuth) {
			t.Fatalf("recipients = %+v, want %s", code.Recipients, userAuth)
		}
		if len(code.Code) != 6 || !code.ExpireTime.After(server.NowUtc()) {
			t.Fatalf("code = %+v", code)
		}

		// a wrong code changes nothing
		if _, err := DisconnectStWallets(ctx, settings, userId, networkId, accountCodeTestWrong(code.Code)); !errors.Is(err, ErrStWalletDisconnectCodeInvalid) {
			t.Fatalf("a wrong code: %v", err)
		}
		if GetStWallet(ctx, networkId) == nil {
			t.Fatal("a wrong code removed the side copy")
		}

		disconnected, err := DisconnectStWallets(ctx, settings, userId, networkId, " "+code.Code[:3]+"-"+code.Code[3:]+" ")
		if err != nil {
			t.Fatal(err)
		}
		if !disconnected.NetworkWallet || disconnected.ProviderWalletCount != 1 {
			t.Fatalf("disconnected = %+v", disconnected)
		}

		// the apps' wallet read (GET /sn/wallet) sees none
		if wallet := GetStWallet(ctx, networkId); wallet != nil {
			t.Fatalf("the side copy remains: %+v", wallet)
		}
		if wallets := GetStProviderWalletsForNetwork(ctx, networkId); len(wallets) != 0 {
			t.Fatalf("the network still lists provider wallets: %+v", wallets)
		}
		if HasDisconnectableStWallet(ctx, networkId) {
			t.Fatal("the network is still disconnectable")
		}
		// settlement reads the provider unmapped (the zero key) from now on
		boundary := server.NowUtc().Add(time.Second)
		history := GetStProviderWalletsAt(ctx, boundary)
		if wallet := history[clientId]; wallet == nil || wallet.ColdkeyPubkey != ([32]byte{}) || wallet.NetworkId != networkId {
			t.Fatalf("the provider's account history at the epoch end = %+v, want the zero key", wallet)
		}
		// another network's provider is untouched
		if wallet := history[otherClientId]; wallet == nil || wallet.ColdkeyPubkey != otherKey {
			t.Fatalf("another network's provider = %+v", wallet)
		}
		// the security notice owed with the change lists both removals (a
		// provider's set also wrote the side copy, so it was that provider's key)
		if !stDisconnectTestNoticeListsRemoval(t, ctx, networkId, SecurityWalletDestinationSubnetNetwork, providerAddress) {
			t.Fatal("no payout wallet notice lists the side copy's removal")
		}
		if !stDisconnectTestNoticeListsRemoval(t, ctx, networkId, SecurityWalletDestinationSubnetProvider, providerAddress) {
			t.Fatal("no payout wallet notice lists the provider wallet's removal")
		}

		// the code confirmed one disconnect
		if _, err := DisconnectStWallets(ctx, settings, userId, networkId, code.Code); !errors.Is(err, ErrStWalletDisconnectCodeExpired) {
			t.Fatalf("the used code again: %v", err)
		}
		// and with nothing left, no new code is sent
		if _, err := SendStWalletDisconnectCode(ctx, settings, userId, networkId); !errors.Is(err, ErrStWalletDisconnectNoWallet) {
			t.Fatalf("a code with nothing to disconnect: %v", err)
		}

		// a wallet connected again pays again
		newKey, newAddress := stDisconnectTestColdkey(t)
		SetStWalletOriginal(ctx, networkId, &clientId, newAddress, newKey, nil, nil)
		if wallet := GetStProviderWalletsAt(ctx, server.NowUtc().Add(time.Second))[clientId]; wallet == nil || wallet.ColdkeyPubkey != newKey {
			t.Fatalf("the reconnected provider = %+v", wallet)
		}
		if wallets := GetStProviderWalletsForNetwork(ctx, networkId); len(wallets) != 1 || wallets[0].ColdkeyPubkey != newKey {
			t.Fatalf("the reconnected network lists %+v", wallets)
		}
	})
}

// Only the network's admin may ask for a code or disconnect, a code goes only
// to the account's sign-in email (an account without one is refused), and a
// network's code never disconnects another network.
func TestStWalletDisconnectRefusesOthers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultStWalletDisconnectSettings()
		networkId, userId, _ := stDisconnectTestNetwork(t, ctx)
		otherNetworkId, otherUserId, _ := stDisconnectTestNetwork(t, ctx)
		key, address := stDisconnectTestColdkey(t)
		SetStWalletOriginal(ctx, networkId, nil, address, key, nil, nil)
		otherKey, otherAddress := stDisconnectTestColdkey(t)
		SetStWalletOriginal(ctx, otherNetworkId, nil, otherAddress, otherKey, nil, nil)

		// a user who does not administer the network
		if _, err := SendStWalletDisconnectCode(ctx, settings, otherUserId, networkId); !errors.Is(err, ErrStWalletDisconnectOwnerOnly) {
			t.Fatalf("another user's code: %v", err)
		}

		// the other network's owner asks for a code; it never disconnects this
		// network, whose own owner asked for none
		otherCode, err := SendStWalletDisconnectCode(ctx, settings, otherUserId, otherNetworkId)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DisconnectStWallets(ctx, settings, otherUserId, networkId, otherCode.Code); !errors.Is(err, ErrStWalletDisconnectCodeExpired) {
			t.Fatalf("another network's code: %v", err)
		}
		if _, err := DisconnectStWallets(ctx, settings, userId, networkId, otherCode.Code); !errors.Is(err, ErrStWalletDisconnectCodeExpired) {
			t.Fatalf("a code the owner never asked for: %v", err)
		}
		if GetStWallet(ctx, networkId) == nil {
			t.Fatal("the network's wallet was removed")
		}

		// an account with no sign-in email gets no code
		guestNetworkId := server.NewId()
		guestUserId := server.NewId()
		Testing_CreateLegacyGuestNetwork(ctx, guestNetworkId, guestUserId)
		guestKey, guestAddress := stDisconnectTestColdkey(t)
		SetStWalletOriginal(ctx, guestNetworkId, nil, guestAddress, guestKey, nil, nil)
		if _, err := SendStWalletDisconnectCode(ctx, settings, guestUserId, guestNetworkId); !errors.Is(err, ErrStWalletDisconnectEmailRequired) {
			t.Fatalf("an account without an email: %v", err)
		}
		if GetStWallet(ctx, guestNetworkId) == nil {
			t.Fatal("the account without an email lost its wallet")
		}
	})
}

// A code takes TryLimit wrong tries and then ends, even for the right code,
// and an account gets CodeSendLimit codes in the window.
func TestStWalletDisconnectBoundsTriesAndCodes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultStWalletDisconnectSettings()
		settings.CodeSendLimit = 2
		networkId, userId, _ := stDisconnectTestNetwork(t, ctx)
		key, address := stDisconnectTestColdkey(t)
		SetStWalletOriginal(ctx, networkId, nil, address, key, nil, nil)

		code, err := SendStWalletDisconnectCode(ctx, settings, userId, networkId)
		if err != nil {
			t.Fatal(err)
		}
		wrong := accountCodeTestWrong(code.Code)
		for i := 0; i < settings.TryLimit-1; i += 1 {
			if _, err := DisconnectStWallets(ctx, settings, userId, networkId, wrong); !errors.Is(err, ErrStWalletDisconnectCodeInvalid) {
				t.Fatalf("wrong try %d: %v", i, err)
			}
		}
		if _, err := DisconnectStWallets(ctx, settings, userId, networkId, wrong); !errors.Is(err, ErrStWalletDisconnectCodeExpired) {
			t.Fatalf("the last wrong try: %v", err)
		}
		if _, err := DisconnectStWallets(ctx, settings, userId, networkId, code.Code); !errors.Is(err, ErrStWalletDisconnectCodeExpired) {
			t.Fatalf("the right code after the tries were spent: %v", err)
		}
		if GetStWallet(ctx, networkId) == nil {
			t.Fatal("the wallet was removed")
		}

		// the second code of the window replaces the first
		second, err := SendStWalletDisconnectCode(ctx, settings, userId, networkId)
		if err != nil {
			t.Fatal(err)
		}
		// the third is refused, with its wait
		_, err = SendStWalletDisconnectCode(ctx, settings, userId, networkId)
		var rateLimit interface{ RetryAfterSeconds() int }
		if !errors.As(err, &rateLimit) || rateLimit.RetryAfterSeconds() <= 0 {
			t.Fatalf("the third code of the window: %v", err)
		}
		// the refusal left the second code working
		if _, err := DisconnectStWallets(ctx, settings, userId, networkId, second.Code); err != nil {
			t.Fatal(err)
		}
	})
}
