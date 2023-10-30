package router

import (
    "context"
    "net/http"
    "encoding/json"
    "fmt"

    "bringyour.com/bringyour"
)


func WarpStatus(w http.ResponseWriter, r *http.Request) {
    type WarpStatusResult struct {
        Version *string `json:"version,omitempty"`
        ConfigVersion *string `json:"configVersion,omitempty"`
        Status string `json:"status"`
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

    status, err := collectStatus(r.Context())
    if err != nil {
        status = fmt.Sprintf("error: %s", err.Error())
    }

    result := &WarpStatusResult{
        Version: warpVersion,
        ConfigVersion: warpConfigVersion,
        Status: status,
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
    err := bringyour.Db(ctx, func(conn bringyour.PgConn) {
        dbError = conn.Ping(ctx)
    })
    if err != nil {
        return "", err
    }
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
