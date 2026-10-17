package model

import (
	"time"

	"github.com/urnetwork/server/v2026"
)

// A sequential Due request owns these numeric observations. Nested phases are
// inclusive wall times and must not be added together. Query phases include
// the client protocol and decoding; they are not server-only execution times.
// Attempts that roll back remain visible, including their retry waits.
type ProviderUrlProbeDueObservation struct {
	Phases            [ProviderUrlProbeDuePhaseCount]server.DbTimingSample
	ClaimDatabase     server.DbTiming
	RetentionDatabase server.DbTiming
}

type ProviderUrlProbeDuePhase uint8

const (
	ProviderUrlProbeDueModel ProviderUrlProbeDuePhase = iota
	ProviderUrlProbeDueClaimTransaction
	ProviderUrlProbeDueClaimBody
	ProviderUrlProbeDueExpiry
	ProviderUrlProbeDueExpiryPending
	ProviderUrlProbeDuePromote
	ProviderUrlProbeDuePromotePending
	ProviderUrlProbeDueClaimQueryRows
	ProviderUrlProbeDueRetentionTransaction
	ProviderUrlProbeDueRetentionQuery
	ProviderUrlProbeDuePhaseCount
)

func (self *ProviderUrlProbeDueObservation) measure(phase ProviderUrlProbeDuePhase, work func()) {
	if self == nil {
		work()
		return
	}
	started := time.Now()
	defer func() {
		self.Phases[phase].Count++
		self.Phases[phase].Duration += time.Since(started)
	}()
	work()
}

func (self *ProviderUrlProbeDueObservation) database(retention bool) *server.DbTiming {
	if self == nil {
		return nil
	}
	if retention {
		return &self.RetentionDatabase
	}
	return &self.ClaimDatabase
}
