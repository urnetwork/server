package quotahost

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// How a lease reaches a payer's balances (DESIGN-DETAILS 13.2.2's reading balances): a BalanceSource
// answers the network's in-window balances in escrowDrawLess's order, each with the credit read then, and
// marks the network spent. NewLocalBalanceSource serves a process with Postgres over model; a host with
// none takes api's internal surface (QD-055), which QUOTA-M2-11's NewApiQuota supplies. The lease set keeps
// each network's last answer with the time it asked for it: a reservation uses an answer younger than
// BalanceRefreshAge as it is, refreshes an older one first, and never uses one older than MaxCreditAge, so
// with the floor (quota_lease_reservation.go) no reading, however old, lets a reservation pass what a
// balance holds. None of it runs on Spend.

// A payer's in-window balance as a lease draws on it.
type LeaseBalance struct {
	BalanceId server.Id
	// balance_byte_count when it was read, 0 when it is negative.
	CreditByteCount int64
	// team_credit: drawn last (D-QUOTA-9).
	IsCredit bool
	EndTime  time.Time
}

// The payer's balances and its spent mark.
type BalanceSource interface {
	// The network's in-window balances in escrowDrawLess's order, each with the credit read then. One read,
	// never cached by the source. An error leaves the caller with what it holds.
	DrawOrder(ctx context.Context, networkId server.Id) ([]*LeaseBalance, error)
	// Marks the network spent (§20.6): model.MarkNetworkSpent, or api's internal route.
	MarkSpent(ctx context.Context, networkId server.Id) error
}

// model's balances for a process with Postgres, read at clock (nil being real time).
func NewLocalBalanceSource(clock sidecar.Clock) BalanceSource {
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	return &localBalanceSource{
		clock: clock,
	}
}

type localBalanceSource struct {
	clock sidecar.Clock
}

func (self *localBalanceSource) DrawOrder(ctx context.Context, networkId server.Id) (balances []*LeaseBalance, returnErr error) {
	server.HandleError(func() {
		for _, balance := range model.LoadDrawOrderBalances(ctx, networkId, self.clock.Now()) {
			balances = append(balances, &LeaseBalance{
				BalanceId:       balance.BalanceId,
				CreditByteCount: max(balance.BalanceByteCount, 0),
				IsCredit:        balance.Credit,
				EndTime:         balance.EndTime,
			})
		}
	}, func(err error) {
		returnErr = err
	})
	return
}

func (self *localBalanceSource) MarkSpent(ctx context.Context, networkId server.Id) (returnErr error) {
	server.HandleError(func() {
		returnErr = model.MarkNetworkSpent(ctx, networkId, self.clock.Now())
	}, func(err error) {
		returnErr = err
	})
	return
}

// One network's last answer from its source.
type leaseBalanceRead struct {
	// when the set asked for it: its age runs from here, not from the answer
	askTime  time.Time
	balances []*LeaseBalance
}

// What the set answers when no reading young enough can be had: the source failed and what it held is
// past MaxCreditAge, so nothing is reserved and nothing marked.
var errLeaseBalancesUnavailable = errors.New("quota lease: no balance reading young enough")

// The network's balances for a reservation now: the held answer while it is younger than
// BalanceRefreshAge, else a fresh read, else, when the read fails, the held answer while it is younger
// than MaxCreditAge. It answers the reading with the time it was asked for. It never runs under the set's
// lock.
func (self *quotaLeaseSet) readBalances(ctx context.Context, networkId server.Id) ([]*LeaseBalance, time.Time, error) {
	settings := self.settings
	now := self.clock.Now()
	var held *leaseBalanceRead
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		held = self.balanceReads[networkId]
	}()
	if held != nil && now.Sub(held.askTime) < settings.BalanceRefreshAge {
		return held.balances, held.askTime, nil
	}
	askTime := now
	callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
	balances, err := self.balances.DrawOrder(callCtx, networkId)
	cancel()
	if err == nil {
		read := &leaseBalanceRead{askTime: askTime, balances: balances}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if current := self.balanceReads[networkId]; current == nil || current.askTime.Before(askTime) {
				self.balanceReads[networkId] = read
			}
		}()
		return balances, askTime, nil
	}
	if held != nil && self.clock.Now().Sub(held.askTime) < settings.MaxCreditAge {
		return held.balances, held.askTime, nil
	}
	return nil, time.Time{}, fmt.Errorf("%w: %w", errLeaseBalancesUnavailable, err)
}

// Drops the readings no reservation may use any more, so a set serving many networks keeps only the live
// ones' answers.
func (self *quotaLeaseSet) forgetOldBalanceReadsWithLock(now time.Time) {
	for networkId, read := range self.balanceReads {
		if self.settings.MaxCreditAge <= now.Sub(read.askTime) {
			delete(self.balanceReads, networkId)
		}
	}
}

// Forgets the network's held reading, so the next reservation reads its source: a test's lever for
// balances it changed by hand.
func (self *quotaLeaseSet) forgetBalanceRead(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.balanceReads, networkId)
}
