package server

// DbOperationPhase identifies a finite source boundary, never SQL, arguments,
// ownership identities or error text. It does not identify the physical cause
// of a failure inside Acquire (such as initial Ping versus pool capacity).
type DbOperationPhase uint8

const (
	DbOperationUnknown DbOperationPhase = iota
	DbOperationOwnershipConfiguration
	DbOperationAcquire
	DbOperationOwnershipAcquire
	DbOperationAdmission
	DbOperationSessionSetup
	DbOperationBegin // BEGIN and admitted-session validation before the business callback.
	DbOperationCallback
	DbOperationCommit
	DbOperationAcknowledged
	DbOperationPostCommit
)

// DbPhaseObservation is an optional, sequentially owned Db/Tx/OwnedTx option.
// The caller reads it only after that invocation unwinds. Cleanup and retry waiting deliberately
// do not replace the last database operation phase. No callback can affect database behavior.
// A phase records source entry, not a completed operation or financial outcome.
type DbPhaseObservation struct {
	phase DbOperationPhase
}

func (self *DbPhaseObservation) enter(phase DbOperationPhase) {
	if self != nil {
		self.phase = phase
	}
}

func (self *DbPhaseObservation) Phase() DbOperationPhase {
	if self == nil {
		return DbOperationUnknown
	}
	return self.phase
}

func dbPhaseObservation(options []any) *DbPhaseObservation {
	var observation *DbPhaseObservation
	for _, option := range options {
		if value, ok := option.(*DbPhaseObservation); ok {
			observation = value
		}
	}
	return observation
}
