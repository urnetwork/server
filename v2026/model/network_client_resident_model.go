package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// the resident is a transport client that runs on the platform on behalf of a client
// there is at most one resident per client, which is self-nominated by any endpoint
// the nomination happens when the endpoint cannot communicate with the current resident

type NetworkClientResident struct {
	ClientId              server.Id `json:"client_id"`
	InstanceId            server.Id `json:"instance_id"`
	ResidentId            server.Id `json:"resident_id"`
	ResidentHost          string    `json:"resident_host"`
	ResidentService       string    `json:"resident_service"`
	ResidentBlock         string    `json:"resident_block"`
	ResidentInternalPorts []int     `json:"resident_internal_ports"`
}

// use gob encoding for `NetworkClientResident` which is more compact than json

func residentKey(clientId server.Id) string {
	return fmt.Sprintf("ncr_%s", clientId)
}

func loadResident(residentBytes []byte) (*NetworkClientResident, error) {
	if len(residentBytes) == 0 {
		return nil, nil
	}
	b := bytes.NewBuffer(residentBytes)
	e := gob.NewDecoder(b)
	var resident NetworkClientResident
	err := e.Decode(&resident)
	if err != nil {
		return nil, err
	}
	return &resident, nil
}

func GetResidentForClient(ctx context.Context, clientId server.Id, ttl time.Duration) (resident *NetworkClientResident) {
	server.Redis(ctx, func(r server.RedisClient) {
		key := residentKey(clientId)
		cmd := r.Get(ctx, key)
		residentBytes, _ := cmd.Bytes()
		resident, _ = loadResident(residentBytes)
		if resident != nil && 0 < ttl {
			r.Expire(ctx, key, ttl)
		}
	})
	return
}

func GetResidentForClientWithInstance(ctx context.Context, clientId server.Id, instanceId server.Id, ttl time.Duration) (resident *NetworkClientResident) {
	resident_ := GetResidentForClient(ctx, clientId, ttl)
	if resident_ != nil && resident_.InstanceId == instanceId {
		resident = resident_
	}
	return
}

// replace an existing resident with the given, or if there was already a replacement, return it
func NominateResident(
	ctx context.Context,
	residentIdToReplace *server.Id,
	nomination *NetworkClientResident,
	ttl time.Duration,
) (nominated bool) {
	b := bytes.NewBuffer(nil)
	e := gob.NewEncoder(b)
	e.Encode(nomination)
	nominationBytes := b.Bytes()

	server.Redis(ctx, func(r server.RedisClient) {
		key := residentKey(nomination.ClientId)
		cmd := r.Get(ctx, key)
		residentBytes, _ := cmd.Bytes()
		resident, _ := loadResident(residentBytes)
		var err error
		if resident == nil {
			if len(residentBytes) == 0 {
				cmd := r.SetNX(ctx, key, nominationBytes, ttl)
				nominated, err = cmd.Result()
			} else {
				// replacing a corrupt value
				cmd := server.RedisSetIfEqual(r, ctx, key, residentBytes, nominationBytes, ttl)
				nominated, err = cmd.Bool()
			}
		} else if residentIdToReplace == nil {
			if resident.InstanceId != nomination.InstanceId {
				cmd := server.RedisSetIfEqual(r, ctx, key, residentBytes, nominationBytes, ttl)
				nominated, err = cmd.Bool()
			}
		} else {
			if resident.InstanceId != nomination.InstanceId || resident.ResidentId == *residentIdToReplace {
				cmd := server.RedisSetIfEqual(r, ctx, key, residentBytes, nominationBytes, ttl)
				nominated, err = cmd.Bool()
			}
		}
		if err != nil {
			glog.Infof("[ncrm]nominated=%t err=%v\n", nominated, err)
		}
	})
	return
}

func RemoveResidentForClient(
	ctx context.Context,
	clientId server.Id,
	residentId server.Id,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		key := residentKey(clientId)
		cmd := r.Get(ctx, key)
		residentBytes, _ := cmd.Bytes()
		resident, _ := loadResident(residentBytes)
		if resident != nil && resident.ResidentId == residentId {
			server.RedisRemoveIfEqual(r, ctx, key, residentBytes)
		}
	})
}
