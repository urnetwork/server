// Receipt absence and disproved inclusion are completed observations. Failed
// transport or integrity reads remain unknown even beside an orphaned receipt.
package controller

// Only the reconciler creates this diagnostic after a successful Rpc read.
// Its text is never used to classify untrusted endpoint failures as absence.
type stOrphanedTransactionReceiptError struct {
	message string
}

// Keeps the existing orphan diagnostic while carrying its completed-read fact.
func (self *stOrphanedTransactionReceiptError) Error() string {
	return self.message
}

// Every leaf must establish absence. A single successful orphan observation
// cannot hide another candidate's failed read through a wrapper or joined error.
func stTransactionReceiptCensusComplete(err error) bool {
	if err == nil {
		return true
	}
	if _, orphan := err.(*stOrphanedTransactionReceiptError); orphan {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !stTransactionReceiptCensusComplete(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		return cause != nil && stTransactionReceiptCensusComplete(cause)
	}
	return false
}
