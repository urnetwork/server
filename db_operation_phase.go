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
	phase     DbOperationPhase
	admission DbAdmissionStage
}

// DbAdmissionStage is the last entered pre-BEGIN ownership operation. A busy
// wait follows an acknowledged refusal and successful cleanup; it does not
// identify a key or prove that the same key remained held during the wait.
type DbAdmissionStage uint8

const (
	DbAdmissionUnknown DbAdmissionStage = iota
	DbAdmissionPrecheck
	DbAdmissionProbe
	DbAdmissionCleanup
	DbAdmissionAcknowledgedBusyWait
)

func (self *DbPhaseObservation) enterAdmission(stage DbAdmissionStage) {
	if self != nil {
		self.admission = stage
	}
}

// AdmissionStage is meaningful only while Phase reports DbOperationAdmission.
// Probe includes the Query, row decoding, and terminal Rows.Err reply checks.
func (self *DbPhaseObservation) AdmissionStage() DbAdmissionStage {
	if self == nil {
		return DbAdmissionUnknown
	}
	return self.admission
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
