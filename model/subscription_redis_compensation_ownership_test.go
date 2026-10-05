package model

import (
	"slices"
	"sync"
	"testing"

	"github.com/urnetwork/server"
)

func TestRedisCompensationKnownZeroKeepsConcurrentUnknown(t *testing.T) {
	owner := &redisContractAdmission{contractId: server.NewId()}
	balance := server.NewId()
	// One positive or uncertain earlier attempt remains owned while all other
	// concurrent attempts conclusively reserve zero on the same balance.
	owner.noteBalance(balance)
	var joined sync.WaitGroup
	for range 128 {
		joined.Go(func() {
			owner.noteBalance(balance)
			owner.noteZeroReservation(balance)
		})
	}
	joined.Wait()
	if ids := owner.compensationIds(); !slices.Equal(ids, []server.Id{balance}) {
		t.Fatal("concurrent zero replies erased another attempt's ownership", ids)
	}
	if !slices.Equal(owner.attemptedBalanceIds, []server.Id{balance}) {
		t.Fatal("concurrent attempts duplicated or dropped discovery state")
	}
	owner.notePublication()
	owner.noteBalance(balance)
	if len(owner.compensationIds()) != 0 {
		t.Fatal("a retry cleared the sticky publication fence")
	}
}

func TestRedisCompensationKnownZeroKeepsSeededUnknown(t *testing.T) {
	balance := server.NewId()
	owner := &redisContractAdmission{contractId: server.NewId(), attemptedBalanceIds: []server.Id{balance}}
	if ids := owner.compensationIds(); !slices.Equal(ids, []server.Id{balance}) {
		t.Fatal("unclassified legacy state lost its compensation obligation")
	}
	owner.noteBalance(balance)
	owner.noteZeroReservation(balance)
	if ids := owner.compensationIds(); !slices.Equal(ids, []server.Id{balance}) {
		t.Fatal("known zero erased preexisting uncertain state")
	}
	zero := server.NewId()
	owner.noteBalance(zero)
	owner.noteZeroReservation(zero)
	if ids := owner.compensationIds(); !slices.Equal(ids, []server.Id{balance}) || !slices.Contains(owner.attemptedBalanceIds, zero) {
		t.Fatal("zero classification changed uncertainty or retained discovery", ids)
	}
}
