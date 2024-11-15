package jwtutil

import (
	"fmt"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect"
)

func ParseClientID(byClientJwt string) (*connect.Id, error) {
	parser := gojwt.NewParser()
	token, _, err := parser.ParseUnverified(byClientJwt, gojwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("failed to parse jwt: %w", err)
	}

	claims := token.Claims.(gojwt.MapClaims)

	clientId, err := connect.ParseId(claims["client_id"].(string))
	if err != nil {
		return nil, fmt.Errorf("failed to parse client id: %w", err)
	}

	return &clientId, nil

}
