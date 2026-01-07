package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

type WarpStatusResult struct {
	Version       *string `json:"version,omitempty"`
	ConfigVersion *string `json:"config_version,omitempty"`
	Status        string  `json:"status"`
	ClientAddress string  `json:"client_address"`
	Host          string  `json:"host"`
	Service       string  `json:"service"`
	Block         string  `json:"block"`
}

func WarpStatus(w http.ResponseWriter, r *http.Request) {
	var warpVersion *string
	if version, err := server.Version(); err == nil {
		warpVersion = &version
	} else {
		warpVersion = nil
	}

	var warpConfigVersion *string
	if configVersion, err := server.ConfigVersion(); err == nil {
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
		Host:          server.RequireHost(),
		Service:       server.RequireService(),
		Block:         server.RequireBlock(),
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
	/*
		// ping postgres
		var dbError error
		server.Db(ctx, func(conn server.PgConn) {
			dbError = conn.Ping(ctx)
		})
		if dbError != nil {
			return "", dbError
		}

		// ping redis
		var redisError error
		server.Redis(ctx, func(client server.RedisClient) {
			redisError = client.Ping(ctx).Err()
		})
		if redisError != nil {
			return "", redisError
		}
	*/

	return "ok", nil
}
