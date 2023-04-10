package model

import (
	"crypto/rand"
	"encoding/hex"

	"golang.org/x/crypto/argon2"

	// "bringyour.com/bringyour"
	// "bringyour.com/bringyour/ulid"
)


type UserAuthType string

const (
	UserAuthTypeNone UserAuthType = "none"
	UserAuthTypeEmail UserAuthType = "email"
	UserAuthTypePhone UserAuthType = "phone"
)


// BE CAREFUL do not change without a backwards-compatible migration
var passwordPepper = []byte("t1me4atoporita")


// BE CAREFUL do not change without a backwards-compatible migration
func NormalUserAuthV1(userAuth *string) (*string, UserAuthType) {

	return nil, UserAuthTypeNone	
}

// BE CAREFUL do not change without a backwards-compatible migration
func computePasswordHashV1(password []byte, passwordSalt []byte) []byte {
	pepperedPassword := []byte{}
	pepperedPassword = append(pepperedPassword, passwordPepper...)
	pepperedPassword = append(pepperedPassword, password...)
	// use RFC recommendations from https://pkg.go.dev/golang.org/x/crypto/argon2
	// 3 seconds
	// 32MiB memory
	// 32 byte key length
	passwordHash := argon2.Key(pepperedPassword, passwordSalt, 3, 32*1024, 4, 32)
	return passwordHash
}

func createPasswordSalt() []byte {
	passwordSalt := make([]byte, 32)
	_, err := rand.Read(passwordSalt)
	if err != nil {
		panic(err)
	}
	return passwordSalt
}

func createValidateCode() string {
	validateCode := make([]byte, 4)
	_, err := rand.Read(validateCode)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(validateCode)
}

func createResetCode() string {
	resetCode := make([]byte, 64)
	_, err := rand.Read(resetCode)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(resetCode)
}




