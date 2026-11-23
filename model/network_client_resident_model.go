package model

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
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

// A `NetworkClientResident` record is gob, which is more compact than json, or
// the typed format (network_client_resident_codec.go). Readers read both.

// The text of the key without fmt: every resident lookup builds it, and fmt
// boxed the id and ran its printer for it (CPUOPT7.md §12).
func residentKey(clientId server.Id) string {
	return "ncr_" + clientId.String()
}

// The resident of a record in either format, nil for an empty one. A record
// that cannot be read is an error.
func loadResident(residentBytes []byte) (*NetworkClientResident, error) {
	if len(residentBytes) == 0 {
		return nil, nil
	}
	return decodeNetworkClientResident(residentBytes)
}

// Testing_ResidentRecord is a client's resident record as stored, in its
// format, for tests of the writers.
func Testing_ResidentRecord(ctx context.Context, clientId server.Id) []byte {
	var record []byte
	server.Redis(ctx, func(r server.RedisClient) {
		record, _ = r.Get(ctx, residentKey(clientId)).Bytes()
	})
	return record
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

// replace an existing resident with the given, or if there was already a replacement, return it.
// The nomination is written as gob, which every reader reads.
func NominateResident(
	ctx context.Context,
	residentIdToReplace *server.Id,
	nomination *NetworkClientResident,
	ttl time.Duration,
) (nominated bool) {
	return NominateResidentWithFormat(ctx, residentIdToReplace, nomination, ttl, NetworkClientResidentFormatGob)
}

// NominateResident with the nomination written in format. Connect chooses the
// format by its settings (connect/exchange_record_format.go). The record it
// replaces may be in either format: a replacement compares the exact bytes it
// read, so it replaces only the record it read, whatever its format.
func NominateResidentWithFormat(
	ctx context.Context,
	residentIdToReplace *server.Id,
	nomination *NetworkClientResident,
	ttl time.Duration,
	format NetworkClientResidentFormat,
) (nominated bool) {
	nominationBytes := encodeNetworkClientResident(format, nomination)

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
			glog.V(1).Infof("[ncrm]nominated=%t err=%v\n", nominated, err)
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

// RemoveResidentForClientWithDeadline retires an abandoned admission without
// keeping its constructor alive through ordinary Redis retries. The exact
// encoded-value comparison cannot delete a newer resident generation. Failure
// leaves the existing TTL to retire the optional registration.
func RemoveResidentForClientWithDeadline(
	ctx context.Context,
	clientId server.Id,
	residentId server.Id,
) error {
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		key := residentKey(clientId)
		residentBytes, err := r.Get(ctx, key).Bytes()
		if err == server.RedisNil {
			return nil
		}
		if err != nil {
			return err
		}
		resident, err := loadResident(residentBytes)
		if err != nil {
			return err
		}
		if resident == nil || resident.ResidentId != residentId {
			return nil
		}
		// This conditional deletion declares exactly one Redis key.
		return server.RedisRemoveIfEqual(r, ctx, key, residentBytes).Err()
	})
}
