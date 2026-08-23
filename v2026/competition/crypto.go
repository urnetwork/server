package competition

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/urnetwork/server/v2026"
)

const roundCommitmentDomain = "urnetwork-sim-latency-round-v1\x00"

func createRoundSecret(settings *Settings, roundId server.Id) (nonce, ciphertext []byte, commitment string, err error) {
	seed := make([]byte, 32)
	if _, err = rand.Read(seed); err != nil {
		return nil, nil, "", err
	}
	block, err := aes.NewCipher(settings.SeedKey)
	if err != nil {
		return nil, nil, "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, "", err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, "", err
	}
	aad := roundAAD(settings, roundId)
	ciphertext = gcm.Seal(nil, nonce, seed, aad)
	h := sha256.New()
	h.Write([]byte(roundCommitmentDomain))
	h.Write(roundId.Bytes())
	h.Write(seed)
	commitment = hex.EncodeToString(h.Sum(nil))
	clear(seed)
	return nonce, ciphertext, commitment, nil
}

func revealRoundSecret(settings *Settings, round *roundRecord) (string, error) {
	block, err := aes.NewCipher(settings.SeedKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(round.SeedNonce) != gcm.NonceSize() {
		return "", fmt.Errorf("round nonce has invalid length")
	}
	seed, err := gcm.Open(nil, round.SeedNonce, round.SeedCiphertext, roundAAD(settings, round.RoundId))
	if err != nil {
		return "", fmt.Errorf("decrypt round seed: %w", err)
	}
	defer clear(seed)
	if len(seed) != 32 {
		return "", fmt.Errorf("round seed has invalid length")
	}
	h := sha256.New()
	h.Write([]byte(roundCommitmentDomain))
	h.Write(round.RoundId.Bytes())
	h.Write(seed)
	if hex.EncodeToString(h.Sum(nil)) != round.WorkloadCommitment {
		return "", fmt.Errorf("round seed does not match commitment")
	}
	return hex.EncodeToString(seed), nil
}

func roundAAD(settings *Settings, roundId server.Id) []byte {
	return []byte(roundCommitmentDomain + settings.CompetitionId + "\x00" + roundId.String() + "\x00" + settings.BaseSha)
}
