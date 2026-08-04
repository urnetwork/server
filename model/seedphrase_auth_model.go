package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"

	bip39 "github.com/tyler-smith/go-bip39"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
)

type SeedphraseLoginResult struct {
	ByJwt string
}

func normalizeSeedphrase(seedphrase string) string {
	words := strings.Fields(strings.ToLower(strings.TrimSpace(seedphrase)))
	return strings.Join(words, " ")
}

func computeSeedphraseLookup(normalized string) []byte {
	h := sha256.Sum256([]byte(normalized))
	return h[:]
}

func CreateSeedphraseAuthInTx(
	tx server.PgTx,
	ctx context.Context,
	userId server.Id,
	seedphrase string,
) error {
	normalized := normalizeSeedphrase(seedphrase)
	lookup := computeSeedphraseLookup(normalized)
	salt := createSeedphraseSalt()
	hash := computeSeedphraseHash([]byte(normalized), salt)

	_, err := tx.Exec(
		ctx,
		`INSERT INTO network_user_auth_seedphrase
		 (user_id, seedphrase_lookup, seedphrase_hash, seedphrase_salt)
		 VALUES ($1, $2, $3, $4)`,
		userId, lookup, hash, salt,
	)
	return err
}

func LoginWithSeedphrase(
	ctx context.Context,
	seedphrase string,
) (*SeedphraseLoginResult, error) {
	normalized := normalizeSeedphrase(seedphrase)
	lookup := computeSeedphraseLookup(normalized)

	var userId server.Id
	var hash []byte
	var salt []byte

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT user_id, seedphrase_hash, seedphrase_salt
			 FROM network_user_auth_seedphrase
			 WHERE seedphrase_lookup = $1`,
			lookup,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&userId, &hash, &salt))
			}
		})
	})

	if userId == (server.Id{}) {
		return nil, errors.New("unknown seedphrase")
	}

	expectedHash := computeSeedphraseHash([]byte(normalized), salt)
	if !bytes.Equal(hash, expectedHash) {
		return nil, errors.New("unknown seedphrase")
	}

	var networkId server.Id
	var networkName string

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT n.network_id, n.network_name
			 FROM network_user nu
			 INNER JOIN network n ON n.admin_user_id = nu.user_id
			 WHERE nu.user_id = $1`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId, &networkName))
			}
		})
	})

	if networkId == (server.Id{}) {
		// The seedphrase auth row outlived its network_user (e.g. the
		// network was deleted but the auth row wasn't cleaned up by some
		// other path). jwt.NewByJwt panics on a zero network id, so fail
		// cleanly here instead of turning an orphaned-row edge case into
		// a 500.
		return nil, errors.New("unknown seedphrase")
	}

	isPro := IsPro(ctx, &networkId)

	byJwt := jwt.NewByJwt(networkId, userId, networkName, false, isPro)
	return &SeedphraseLoginResult{ByJwt: byJwt.Sign()}, nil
}

func RegenerateSeedphrase(ctx context.Context, userId server.Id) (string, error) {
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", err
	}
	newSeedphrase, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return "", err
	}

	normalized := normalizeSeedphrase(newSeedphrase)
	lookup := computeSeedphraseLookup(normalized)
	salt := createSeedphraseSalt()
	hash := computeSeedphraseHash([]byte(normalized), salt)

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_user_auth_seedphrase
			 SET seedphrase_lookup = $2, seedphrase_hash = $3, seedphrase_salt = $4
			 WHERE user_id = $1`,
			userId, lookup, hash, salt,
		))
	})

	return newSeedphrase, nil
}

func GenerateSeedphrase(ctx context.Context, userId server.Id) (string, error) {
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", err
	}
	seedphrase, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return "", err
	}

	server.Tx(ctx, func(tx server.PgTx) {
		err = CreateSeedphraseAuthInTx(tx, ctx, userId, seedphrase)
		server.Raise(err)
	})

	return seedphrase, nil
}

func HasSeedphraseAuth(ctx context.Context, userId server.Id) (bool, error) {
	exists := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT 1 FROM network_user_auth_seedphrase WHERE user_id = $1`,
			userId,
		)
		server.WithPgResult(result, err, func() {
			exists = result.Next()
		})
	})
	return exists, nil
}
