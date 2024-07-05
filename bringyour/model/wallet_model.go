package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	// "time"
	// "fmt"
	// "math"
	// "crypto/rand"
	// "encoding/hex"

	"bringyour.com/bringyour"
	// "bringyour.com/bringyour/session"
)

// this user id is what is used for the Circle api:
// - create a user token
// - list wallets
// - create a wallet challenge
// https://developers.circle.com/w3s/reference
func GetOrCreateCircleUserId(
    ctx context.Context,
    networkId bringyour.Id,
    userId bringyour.Id,
) (circleUserId bringyour.Id) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        result, err := tx.Query(
            ctx,
            `
                SELECT
                    circle_uc_user_id
                FROM circle_uc
                WHERE
                    network_id = $1 AND
                    user_id = $2
            `,
            networkId,
            userId,
        )
        set := false
        bringyour.WithPgResult(result, err, func() {
            if result.Next() {
                bringyour.Raise(result.Scan(&circleUserId))
                set = true
            }
        })

        if set {
            return
        }

        circleUserId = bringyour.NewId()

        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                INSERT INTO circle_uc (
                    network_id,
                    user_id,
                    circle_uc_user_id
                )
                VALUES ($1, $2, $3)
            `,
            networkId,
            userId,
            circleUserId,
        ))
    })
    return
}


// used for testing
func SetCircleUserId(
    ctx context.Context,
    networkId bringyour.Id,
    userId bringyour.Id,
    circleUserId bringyour.Id,
) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
        bringyour.RaisePgResult(tx.Exec(
            ctx,
            `
                INSERT INTO circle_uc (
                    network_id,
                    user_id,
                    circle_uc_user_id
                )
                VALUES ($1, $2, $3)
                ON CONFLICT (network_id, user_id) DO UPDATE
                SET
                	circle_uc_user_id = $3
            `,
            networkId,
            userId,
            circleUserId,
        ))
    })
}

type WalletValidateAddressArgs struct {
    Address string `json:"address,omitempty"`
    Chain   string `json:"chain,omitempty"` // https://developers.circle.com/w3s/reference/createvalidateaddress for valid blockchain params
}

type WalletValidateAddressResult struct {
    Valid bool `json:"valid,omitempty"`
}

func WalletValidateAddress(
    walletValidateAddress *WalletValidateAddressArgs,
) (*WalletValidateAddressResult, error) {
    return bringyour.HttpPostRequireStatusOk(
        "https://api.circle.com/v1/w3s/transactions/validateAddress",
        map[string]any{
            "blockchain": walletValidateAddress.Chain,
            "address": walletValidateAddress.Address,
        },
        func(header http.Header) {
            header.Add("Accept", "application/json")
            header.Add("Authorization", fmt.Sprintf("Bearer %s", CircleConfig()["api_token"]))
        },
        func(response *http.Response, responseBodyBytes []byte)(*WalletValidateAddressResult, error) {
            _, data, err := ParseCircleResponseData(responseBodyBytes)
            if err != nil {
                return nil, err
            }

            valid := false
            if validAny := data["isValid"]; validAny != nil {
                if v, ok := validAny.(bool); ok {
                    valid = v
                } 
            }

            return &WalletValidateAddressResult{
                Valid: valid,
            }, nil
        },
    )
}

var CircleConfig = sync.OnceValue(func() map[string]any {
    c := bringyour.Vault.RequireSimpleResource("circle.yml").Parse()
    return c["circle"].(map[string]any)
})

func ParseCircleResponseData(responseBodyBytes []byte) (
    responseBody map[string]any,
    resultData map[string]any,
    resultErr error,
) {
    responseBody = map[string]any{}
    err := json.Unmarshal(responseBodyBytes, &responseBody)
    if err != nil {
        resultErr = err
        return
    }

    if data, ok := responseBody["data"]; ok {
        switch v := data.(type) {
        case map[string]any:
            resultData = v
            return
        }
    }
    err = fmt.Errorf("Response is missing data.")
    return
}