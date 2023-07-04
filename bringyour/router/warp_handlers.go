package router

import (
    "net/http"
    "encoding/json"
    "fmt"
    "context"

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

    status, err := collectStatus()
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


func collectStatus() (string, error) {
    // ping postgres
    var dbError error
    bringyour.Db(func(ctx context.Context, conn bringyour.PgConn) {
        dbError = conn.Ping(ctx)
    })
    if dbError != nil {
        return "", dbError
    }

    // ping redis
    var redisError error
    bringyour.Redis(func(ctx context.Context, client bringyour.RedisClient) {
        redisError = client.Ping(ctx).Err()
    })
    if redisError != nil {
        return "", redisError
    }
    
    return "ok", nil
}
