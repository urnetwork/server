package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
)

func WarpStatus(w http.ResponseWriter, r *http.Request) {
	type WarpStatusResult struct {
		Version       *string `json:"version,omitempty"`
		ConfigVersion *string `json:"config_version,omitempty"`
		Status        string  `json:"status"`
		ClientAddress string  `json:"client_address"`
		Host          string  `json:"host"`
	}

	var warpVersion *string
	if version, err := bringyour.Version(); err == nil {
		warpVersion = &version
	} else {
		warpVersion = nil
	}

	var warpConfigVersion *string
	if configVersion, err := bringyour.ConfigVersion(); err == nil {
		warpConfigVersion = &configVersion
	} else {
		warpConfigVersion = nil
	}

	var clientAddress string
	if session, err := session.NewClientSessionFromRequest(r); err == nil {
		clientAddress = session.ClientAddress
	} else {
		clientAddress = fmt.Sprintf("error: %s", err.Error())
	}

	status, err := collectStatus(r.Context())
	if err != nil {
		status = fmt.Sprintf("error: %s", err.Error())
	}

	result := &WarpStatusResult{
		Version:       warpVersion,
		ConfigVersion: warpConfigVersion,
		Status:        status,
		ClientAddress: clientAddress,
		Host:          bringyour.RequireHost(),
	}

	responseJson, err := json.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(responseJson)
}

func collectStatus(ctx context.Context) (string, error) {
	// ping postgres
	var dbError error
	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		dbError = conn.Ping(ctx)
	})
	if dbError != nil {
		return "", dbError
	}

	// ping redis
	var redisError error
	bringyour.Redis(ctx, func(client bringyour.RedisClient) {
		redisError = client.Ping(ctx).Err()
	})
	if redisError != nil {
		return "", redisError
	}

	return "ok", nil
}
