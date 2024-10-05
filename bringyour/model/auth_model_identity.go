package model

import (
	"crypto/rand"
	"encoding/hex"
	"net/mail"
	"strings"

	// "github.com/golang/glog"

	"github.com/nyaruka/phonenumbers"
	"golang.org/x/crypto/argon2"
	// "bringyour.com/bringyour"
)

type UserAuthType string

const (
	UserAuthTypeNone  UserAuthType = "none"
	UserAuthTypeEmail UserAuthType = "email"
	UserAuthTypePhone UserAuthType = "phone"
)

// BE CAREFUL do not change without a backwards-compatible migration
var passwordPepper = []byte("t1me4atoporita")

// BE CAREFUL do not change without a backwards-compatible migration
func NormalUserAuthV1(userAuth *string) (*string, UserAuthType) {
	if userAuth == nil {
		return nil, UserAuthTypeNone
	}

	// bringyour.Logger().Printf("Evaluating user auth %s\n", *userAuth)

	normalUserAuth := strings.TrimSpace(*userAuth)
	normalUserAuth = strings.ToLower(normalUserAuth)

	var emailAddress *mail.Address
	var phoneNumber *phonenumbers.PhoneNumber
	var err error

	emailAddress, err = mail.ParseAddress(normalUserAuth)
	if err == nil {
		normalEmailAddress := emailAddress.Address
		// bringyour.Logger().Printf("Parsed email %s\n", normalEmailAddress)
		return &normalEmailAddress, UserAuthTypeEmail
	}

	phoneNumber, err = phonenumbers.Parse(normalUserAuth, "US")
	if err == nil && phonenumbers.IsPossibleNumber(phoneNumber) {
		normalPhoneNumber := phonenumbers.Format(phoneNumber, phonenumbers.INTERNATIONAL)
		// bringyour.Logger().Printf("Parsed phone %s\n", normalPhoneNumber)
		return &normalPhoneNumber, UserAuthTypePhone
	}

	// not recognized
	return nil, UserAuthTypeNone
}

func NormalUserAuth(userAuth string) (string, UserAuthType) {
	normalUserAuth_, userAuthType := NormalUserAuthV1(&userAuth)
	var normalUserAuth string
	if normalUserAuth_ != nil {
		normalUserAuth = *normalUserAuth_
	}
	return normalUserAuth, userAuthType
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

func createVerifyCode() string {
	verifyCode := make([]byte, 4)
	_, err := rand.Read(verifyCode)
	if err != nil {
		panic(err)
	}
	return strings.ToLower(hex.EncodeToString(verifyCode))
}

func Testing_CreateVerifyCode() string {
	return createVerifyCode()
}

func createResetCode() string {
	resetCode := make([]byte, 64)
	_, err := rand.Read(resetCode)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(resetCode)
}
