// Global hotkey wallet mapping consent (sn protocol
// urnetwork-hotkey-wallet-mapping-consent-v1): the coldkey and the hotkey both
// sign it once for every operator of a subnet. This operator only retains it,
// one append-only chain per (subnet, hotkey), submitted complete from
// generation 1 by the owner of a network. It changes no wallet projection and
// earns nothing by itself: a network earns to it only through a hotkey network
// delegation that pins one of its heads (hotkey_network_delegation.go).
//
// The chain needs no challenge and its keys are free to mint, so retention is
// bounded per submitting network: each accepted submission links the network
// to the hotkey, a network links at most MaxHotkeyWalletMappingNetworkHotkeys
// hotkeys, and one submission adds at most
// MaxHotkeyWalletMappingNewGenerations generations.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// Retained chains are keyed by the subnet's fixed-width fields under their own
// tag. The digest never leaves the operator; readers name the subnet.
func hotkeyWalletMappingSubnetHash(subnet protocol.HotkeyWalletMappingSubnet) ([32]byte, error) {
	if err := subnet.Validate(); err != nil {
		return [32]byte{}, errors.Join(protocol.ErrWalletMappingIntegrity, err)
	}
	payload := []byte("urnetwork-hotkey-wallet-mapping-subnet-v1\x00")
	payload = binary.BigEndian.AppendUint64(payload, subnet.ChainID)
	payload = append(payload, subnet.GenesisHash[:]...)
	payload = binary.BigEndian.AppendUint16(payload, subnet.Netuid)
	return sha256.Sum256(payload), nil
}

// Serializing one (subnet, hotkey) keeps two submissions from retaining
// different originals at one generation.
func lockHotkeyWalletMapping(ctx context.Context, tx server.PgTx, subnetHash [32]byte, hotkey [32]byte) {
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "hotkey-wallet-mapping:"+hex.EncodeToString(subnetHash[:])+":"+hex.EncodeToString(hotkey[:])))
}

// Serializing one submitting network keeps two submissions of different new
// hotkeys from both taking its last link. Taken before the chain lock.
func lockHotkeyWalletMappingSubmitter(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "hotkey-wallet-mapping-submitter:"+networkId.String()))
}

// The distinct hotkeys one network may link by submitting their chains.
const MaxHotkeyWalletMappingNetworkHotkeys = 8

// The generations one submission may add beyond what the operator retains. A
// longer chain arrives as successive prefixes of it.
const MaxHotkeyWalletMappingNewGenerations = 64

var ErrHotkeyWalletMappingNetworkHotkeys = fmt.Errorf("A network can store hotkey wallet consent chains for at most %d hotkeys, and this network already has. Submit chains only for the hotkeys it already stores.", MaxHotkeyWalletMappingNetworkHotkeys)
var ErrHotkeyWalletMappingNewGenerations = fmt.Errorf("A hotkey wallet consent submission can add at most %d new generations. Submit a longer chain as successive prefixes.", MaxHotkeyWalletMappingNewGenerations)

// Admits one submission against what the operator retains: the retained
// chain hashes in generation order, the submitted ones, whether the submitting
// network already links the hotkey and how many hotkeys it links. Returns the
// number of generations to append. A different original at a retained
// generation is a fork; a linked hotkey is never refused for the network cap.
func hotkeyWalletMappingSubmission(retained [][32]byte, submitted [][32]byte, linked bool, links int) (int, error) {
	for index := 0; index < len(retained) && index < len(submitted); index += 1 {
		if retained[index] != submitted[index] {
			return 0, protocol.ErrWalletMappingIntegrity
		}
	}
	appended := max(0, len(submitted)-len(retained))
	if MaxHotkeyWalletMappingNewGenerations < appended {
		return 0, ErrHotkeyWalletMappingNewGenerations
	}
	if !linked && MaxHotkeyWalletMappingNetworkHotkeys <= links {
		return 0, ErrHotkeyWalletMappingNetworkHotkeys
	}
	return appended, nil
}

// The head of a submitted chain, all of which the operator now retains.
type HotkeyWalletMappingStored struct {
	Hotkey     [32]byte
	HeadHash   [32]byte
	Generation uint64
	// generations this submission added; zero for a replay
	Appended int
}

// The submitted lineage is verified completely and must name the subnet of
// the owner's domain, and the owner's user must still administer the network.
// Generations the operator does not have yet are appended; one it already has
// must be the identical original, and any other original there is a fork,
// refused with ErrWalletMappingIntegrity. A replay of the chain or of a prefix
// of it retains nothing new and returns the submitted head. Every accepted
// submission links the network to the hotkey; the bounds refuse with
// ErrHotkeyWalletMappingNetworkHotkeys and ErrHotkeyWalletMappingNewGenerations.
func StoreHotkeyWalletMappingChain(ctx context.Context, owner NetworkWalletMappingOwner, originals []protocol.HotkeyWalletMappingConsent) (stored *HotkeyWalletMappingStored, returnErr error) {
	if ctx == nil {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	subnet := owner.Domain.HotkeySubnet()
	subnetHash, err := hotkeyWalletMappingSubnetHash(subnet)
	if err != nil {
		return nil, err
	}
	head, headHash, err := protocol.VerifyHotkeyWalletMappingLineage(ctx, originals)
	if err != nil {
		return nil, err
	}
	if head.Subnet != subnet {
		return nil, protocol.ErrWalletMappingIntegrity
	}
	raws := make([][]byte, len(originals))
	hashes := make([][32]byte, len(originals))
	for index, original := range originals {
		raw, err := json.Marshal(original)
		if err != nil || len(raw) > protocol.MaxWalletMappingConsentBytes {
			return nil, errors.Join(protocol.ErrWalletMappingIntegrity, err)
		}
		raws[index], hashes[index] = raw, sha256.Sum256(raw)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if refusal, ok := recovered.(walletMappingAbort); ok {
				stored, returnErr = nil, refusal.cause
			} else {
				panic(recovered)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		stored = nil
		lockHotkeyWalletMappingSubmitter(ctx, tx, owner.NetworkId)
		if err := checkNetworkWalletMappingOwner(ctx, tx, owner); err != nil {
			panic(walletMappingAbort{cause: err})
		}
		lockHotkeyWalletMapping(ctx, tx, subnetHash, head.Hotkey)
		var links int
		var linked bool
		server.Raise(tx.QueryRow(ctx, `SELECT count(*),coalesce(bool_or(hotkey=$2),false) FROM hotkey_wallet_mapping_submitter WHERE network_id=$1`, owner.NetworkId, head.Hotkey[:]).Scan(&links, &linked))
		var retained [][32]byte
		rows, err := tx.Query(ctx, `SELECT generation,original_hash FROM hotkey_wallet_mapping_consent WHERE subnet_hash=$1 AND hotkey=$2 ORDER BY generation`, subnetHash[:], head.Hotkey[:])
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var generation uint64
				var hash []byte
				server.Raise(rows.Scan(&generation, &hash))
				if generation != uint64(len(retained))+1 || len(hash) != 32 {
					server.Raise(fmt.Errorf("hotkey wallet mapping retained sequence is invalid"))
				}
				retained = append(retained, [32]byte(hash))
			}
		})
		appended, err := hotkeyWalletMappingSubmission(retained, hashes, linked, links)
		if err != nil {
			panic(walletMappingAbort{cause: err})
		}
		now := server.NowUtc()
		if !linked {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO hotkey_wallet_mapping_submitter(network_id,hotkey,create_time) VALUES($1,$2,$3)`, owner.NetworkId, head.Hotkey[:], now))
		}
		for index := len(retained); index < len(originals); index += 1 {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO hotkey_wallet_mapping_consent(subnet_hash,hotkey,generation,original_hash,original,accepted_at) VALUES($1,$2,$3,$4,$5,$6)`, subnetHash[:], head.Hotkey[:], index+1, hashes[index][:], raws[index], now))
		}
		stored = &HotkeyWalletMappingStored{Hotkey: head.Hotkey, HeadHash: headHash, Generation: head.Generation, Appended: appended}
	})
	return stored, nil
}

// Exact-window callers supply a separately pinned head, such as a delegation's
// consent head. This returns the original bytes only; it never declares its
// newest SQL row authoritative.
func ReadHotkeyWalletMappingHistory(ctx context.Context, subnet protocol.HotkeyWalletMappingSubnet, hotkey [32]byte, generation uint64, head [32]byte) ([]protocol.HotkeyWalletMappingConsent, error) {
	if ctx == nil || hotkey == ([32]byte{}) || generation == 0 || generation > protocol.MaxWalletMappingHistory || head == ([32]byte{}) {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	subnetHash, err := hotkeyWalletMappingSubnetHash(subnet)
	if err != nil {
		return nil, err
	}
	owner, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	var result []protocol.HotkeyWalletMappingConsent
	var last []byte
	server.Db(owner, func(conn server.PgConn) {
		rows, err := conn.Query(owner, `SELECT generation,original_hash,original FROM hotkey_wallet_mapping_consent WHERE subnet_hash=$1 AND hotkey=$2 AND generation<=$3 ORDER BY generation`, subnetHash[:], hotkey[:], generation)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var index uint64
				var raw []byte
				server.Raise(rows.Scan(&index, &last, &raw))
				if index != uint64(len(result))+1 || len(raw) > protocol.MaxWalletMappingConsentBytes {
					server.Raise(fmt.Errorf("hotkey wallet mapping retained sequence is invalid"))
				}
				var original protocol.HotkeyWalletMappingConsent
				server.Raise(json.Unmarshal(raw, &original))
				result = append(result, original)
			}
		})
	})
	if err := owner.Err(); err != nil {
		return nil, err
	}
	if uint64(len(result)) != generation || !bytes.Equal(last, head[:]) {
		return nil, protocol.ErrWalletMappingUnavailable
	}
	return result, nil
}
