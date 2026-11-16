// Receipt absence and disproved inclusion are completed observations. Failed
// transport or integrity reads remain unknown even beside an orphaned receipt.
package controller

import "github.com/urnetwork/server/v2026"

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
	causes := server.InspectErrorCauses(err)
	if !causes.Complete {
		return false
	}
	for _, node := range causes.Nodes {
		if node.Leaf {
			if value, orphan := node.Err.(*stOrphanedTransactionReceiptError); !orphan || value == nil {
				return false
			}
		}
	}
	return true
}
