package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/urnetwork/server"
)

// Move intents (connect/MOVE.md §5.3). The source exchange writes one before
// it sends a client a move. When the client's new transport arrives at another
// exchange, that exchange reads the intent and takes over the resident instead
// of forwarding to it. A result report for the move is accepted only while the
// intent holds the reported move (§5.1).
//
// One key per client, `move_intent_<clientId>`, whose ttl is the move's arrival
// window. A single key needs no hash tag under the cluster slot rules.

type ResidentMoveKind string

const (
	// a drain of the source exchange: any other exchange takes over
	ResidentMoveKindDrain ResidentMoveKind = "drain"
	// guidance the client may decline; only the target host takes over
	ResidentMoveKindAdvisory ResidentMoveKind = "advisory"
	// the source closes the client at the deadline; only the target host
	// takes over
	ResidentMoveKindCompulsory ResidentMoveKind = "compulsory"
)

type ResidentMoveIntent struct {
	MoveId         server.Id        `json:"move_id"`
	Kind           ResidentMoveKind `json:"kind"`
	FromResidentId server.Id        `json:"from_resident_id"`
	FromExchangeId server.Id        `json:"from_exchange_id"`
	// the inventory host the client was pointed at
	TargetHost string `json:"target_host"`
	// the addresses the client was sent, at most one per family. A failure
	// report names a family, which picks the address the target breaker counts.
	Targets []netip.Addr `json:"targets,omitempty"`
}

// The target of family 4 or 6. Family 0 is a report that applies to no one
// family: it picks the only target when there is exactly one.
func (self *ResidentMoveIntent) Target(family uint32) (netip.Addr, bool) {
	switch family {
	case 4, 6:
		for _, target := range self.Targets {
			if (family == 4) == target.Unmap().Is4() {
				return target, true
			}
		}
		return netip.Addr{}, false
	case 0:
		if len(self.Targets) == 1 {
			return self.Targets[0], true
		}
		return netip.Addr{}, false
	default:
		return netip.Addr{}, false
	}
}

func residentMoveIntentKey(clientId server.Id) string {
	return fmt.Sprintf("move_intent_%s", clientId)
}

func loadResidentMoveIntent(intentBytes []byte) (*ResidentMoveIntent, error) {
	var intent ResidentMoveIntent
	if err := json.Unmarshal(intentBytes, &intent); err != nil {
		return nil, err
	}
	return &intent, nil
}

// Runs one intent operation, returning a raise as its error. A canceled caller
// ends quietly.
func residentMoveIntentRedis(ctx context.Context, callback func(server.RedisClient) error) (returnErr error) {
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			returnErr = callback(r)
		})
	}, func(err error) {
		returnErr = err
	})
	return
}

// Writes the client's intent, replacing any earlier one, for ttl.
func SetResidentMoveIntent(ctx context.Context, clientId server.Id, intent *ResidentMoveIntent, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("move intent ttl must be positive")
	}
	intentBytes, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return residentMoveIntentRedis(ctx, func(r server.RedisClient) error {
		return r.Set(ctx, residentMoveIntentKey(clientId), intentBytes, ttl).Err()
	})
}

// The client's intent, or nil when there is none.
func GetResidentMoveIntent(ctx context.Context, clientId server.Id) (intent *ResidentMoveIntent, returnErr error) {
	returnErr = residentMoveIntentRedis(ctx, func(r server.RedisClient) error {
		intentBytes, err := r.Get(ctx, residentMoveIntentKey(clientId)).Bytes()
		if errors.Is(err, server.RedisNil) {
			return nil
		}
		if err != nil {
			return err
		}
		intent, err = loadResidentMoveIntent(intentBytes)
		return err
	})
	return
}

// Removes and returns the client's intent (GETDEL), so exactly one arrival
// takes it. Nil when there is none.
func TakeResidentMoveIntent(ctx context.Context, clientId server.Id) (intent *ResidentMoveIntent, returnErr error) {
	returnErr = residentMoveIntentRedis(ctx, func(r server.RedisClient) error {
		intentBytes, err := r.GetDel(ctx, residentMoveIntentKey(clientId)).Bytes()
		if errors.Is(err, server.RedisNil) {
			return nil
		}
		if err != nil {
			return err
		}
		intent, err = loadResidentMoveIntent(intentBytes)
		return err
	})
	return
}

// Removes the client's intent while it is still the intent of moveId, so the
// intent of a newer move survives. Reports whether it removed one.
func DeleteResidentMoveIntent(ctx context.Context, clientId server.Id, moveId server.Id) (deleted bool, returnErr error) {
	returnErr = residentMoveIntentRedis(ctx, func(r server.RedisClient) error {
		key := residentMoveIntentKey(clientId)
		intentBytes, err := r.Get(ctx, key).Bytes()
		if errors.Is(err, server.RedisNil) {
			return nil
		}
		if err != nil {
			return err
		}
		intent, err := loadResidentMoveIntent(intentBytes)
		if err != nil || intent.MoveId != moveId {
			return nil
		}
		// the exact value read, so a replacement written since is kept
		deleted, err = server.RedisRemoveIfEqual(r, ctx, key, intentBytes).Bool()
		return err
	})
	return
}
