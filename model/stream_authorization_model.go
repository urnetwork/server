package model

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"time"
)

func streamAuthorityKey(streamId server.Id, suffix string) string {
	return fmt.Sprintf("{sa_%s}%s", streamId, suffix)
}

type streamEndpointAuthorization struct {
	ClientId   server.Id `json:"client_id"`
	Generation server.Id `json:"generation"`
}
type streamAuthorizationBinding struct {
	Generation server.Id                     `json:"generation"`
	Endpoints  []streamEndpointAuthorization `json:"endpoints"`
	Legacy     bool                          `json:"legacy"`
}
type StreamAuthorization struct {
	Generation         server.Id
	LeaseMillis        uint32
	DeadlineUnixMillis int64
	Allowed            bool
	Legacy             bool
}

// The stream's stable participant set is published before any StreamOpen. All
// resident owners read the same immutable binding through migrations.
func storeStreamParticipants(ctx context.Context, streamId server.Id, key streamKey) error {
	ids := []server.Id{}
	for id := range key.Edges() {
		ids = append(ids, id)
	}
	encoded, _ := json.Marshal(ids)
	return server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.Set(ctx, streamAuthorityKey(streamId, "p"), encoded, 8*time.Hour).Err()
	})
}

// Grants arrive only on platform control. Losing either endpoint authority
// retires this binding permanently; a newer login cannot renew the old stream.
func AuthorizeStream(ctx context.Context, streamId server.Id, now time.Time) (result StreamAuthorization, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var raw, participants string
	var retired bool
	err = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		values, err := r.MGet(ctx, streamAuthorityKey(streamId, "b"), streamAuthorityKey(streamId, "p"), streamAuthorityKey(streamId, "r")).Result()
		if err != nil {
			return err
		}
		if values[0] != nil {
			raw = values[0].(string)
		}
		if values[1] != nil {
			participants = values[1].(string)
		}
		retired = values[2] != nil
		return nil
	})
	if err != nil {
		return
	}
	if raw == "" && participants == "" {
		return
	}
	binding := streamAuthorizationBinding{}
	if raw != "" {
		if err = json.Unmarshal([]byte(raw), &binding); err != nil {
			return
		}
		result.Generation = binding.Generation
		if retired {
			return
		}
	}
	if raw == "" {
		var ids []server.Id
		if err = json.Unmarshal([]byte(participants), &ids); err != nil {
			return
		}
		if len(ids) < 2 || len(ids) > 64 {
			return
		}
		binding.Generation = server.NewId()
		legacy := false
		tagged := false
		for _, id := range ids {
			authority, readErr := session.ReadConnectionAuthority(ctx, id, nil, now)
			if readErr != nil {
				return result, readErr
			}
			if authority == nil {
				return
			}
			legacy = legacy || authority.LeaseVersion < 1
			tagged = tagged || authority.SessionId != nil
			binding.Endpoints = append(binding.Endpoints, streamEndpointAuthorization{id, authority.Generation})
		}
		if legacy && tagged {
			return
		}
		binding.Legacy = legacy
		encoded, _ := json.Marshal(binding)
		err = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			var err error
			raw, err = r.Eval(ctx, `if redis.call('EXISTS',KEYS[3])==1 or redis.call('EXISTS',KEYS[2])==0 then return '' end;redis.call('SET',KEYS[1],ARGV[1],'NX','PX',28800000);return redis.call('GET',KEYS[1])`, []string{streamAuthorityKey(streamId, "b"), streamAuthorityKey(streamId, "p"), streamAuthorityKey(streamId, "r")}, string(encoded)).Text()
			return err
		})
		if err != nil || raw == "" {
			return
		}
		if err = json.Unmarshal([]byte(raw), &binding); err != nil {
			return
		}
	}
	result.Generation = binding.Generation
	result.Legacy = binding.Legacy
	if participants == "" {
		return retireStreamAuthorization(ctx, streamId, result)
	}
	for _, endpoint := range binding.Endpoints {
		authority, readErr := session.ReadConnectionAuthority(ctx, endpoint.ClientId, &endpoint.Generation, now)
		if readErr != nil {
			return result, readErr
		}
		if authority == nil {
			return retireStreamAuthorization(ctx, streamId, result)
		}
		if !binding.Legacy && authority.LeaseVersion < 1 {
			return retireStreamAuthorization(ctx, streamId, result)
		}
	}
	result.Allowed = true
	if !binding.Legacy {
		result.LeaseMillis = uint32(session.AuthorizationLeaseDuration / time.Millisecond)
		result.DeadlineUnixMillis = now.Add(session.AuthorizationLeaseDuration).UnixMilli()
	}
	return
}
func retireStreamAuthorization(ctx context.Context, streamId server.Id, result StreamAuthorization) (StreamAuthorization, error) {
	result.Allowed = false
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.Set(ctx, streamAuthorityKey(streamId, "r"), 1, 8*time.Hour).Err()
	})
	return result, err
}
