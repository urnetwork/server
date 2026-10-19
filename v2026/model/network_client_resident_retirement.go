package model

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server/v2026"
)

// NetworkClientResidentRetirement is an immutable, opaque capture of an exact
// Redis value. Callers cannot manufacture a token for a replacement resident.
// Capture and commit are separate so transport teardown can join between them.
type NetworkClientResidentRetirement struct {
	clientId   server.Id
	instanceId server.Id
	residentId server.Id
	value      []byte
}

// CaptureResidentForClientRetirement reads without extending residency. A
// different instance, missing value or malformed identity is never eligible.
func CaptureResidentForClientRetirement(ctx context.Context, clientId, instanceId server.Id) (*NetworkClientResidentRetirement, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return server.HandleError2(func() (*NetworkClientResidentRetirement, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if clientId == (server.Id{}) || instanceId == (server.Id{}) {
			return nil, errors.New("resident retirement requires client and instance")
		}
		var value []byte
		readErr := server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
			var err error
			value, err = r.Get(ctx, residentKey(clientId)).Bytes()
			return err
		})
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if errors.Is(readErr, server.RedisNil) {
			return nil, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		resident, err := loadResident(value)
		if err != nil {
			return nil, err
		}
		if resident == nil || resident.ClientId != clientId || resident.InstanceId != instanceId || resident.ResidentId == (server.Id{}) {
			return nil, nil
		}
		return &NetworkClientResidentRetirement{
			clientId: clientId, instanceId: instanceId, residentId: resident.ResidentId, value: bytes.Clone(value),
		}, nil
	}, func(err error) (*NetworkClientResidentRetirement, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, err
	})
}

// RemoveCapturedResidentForClient compares the original bytes atomically. It
// never rereads a current resident to choose a target after teardown. Any
// changed generation (including the same instance on a new resident) survives.
func RemoveCapturedResidentForClient(ctx context.Context, token *NetworkClientResidentRetirement) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return server.HandleError2(func() (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if token == nil {
			return false, nil
		}
		if token.clientId == (server.Id{}) || token.instanceId == (server.Id{}) || token.residentId == (server.Id{}) || len(token.value) == 0 {
			return false, errors.New("resident retirement capture is invalid")
		}
		var removed bool
		removeErr := server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
			var err error
			removed, err = server.RedisRemoveIfEqual(r, ctx, residentKey(token.clientId), token.value).Bool()
			return err
		})
		if ctx.Err() != nil {
			return removed, ctx.Err()
		}
		return removed, removeErr
	}, func(err error) (bool, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return false, err
	})
}
