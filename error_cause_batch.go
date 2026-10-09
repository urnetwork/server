// Independently completed rows keep independent cause-inspection budgets.
package server

import "strings"

const errorCauseBatchMaximumMembers = 512

// A batch owns an immutable list of original row errors, not their completeness
// or retry policy. Ordinary inspection still applies its original global cap.
// Explicit batch inspection revalidates every member; nesting is not supported.
type ErrorCauseBatch struct {
	causes []error
}

// The two bounded expiry streams select at most 512 rows. A larger or empty
// batch cannot acquire this finite partition boundary. Nil members stay visible.
func NewErrorCauseBatch(causes []error) *ErrorCauseBatch {
	if len(causes) == 0 || errorCauseBatchMaximumMembers < len(causes) {
		return nil
	}
	return &ErrorCauseBatch{causes: append([]error(nil), causes...)}
}

// Keep the same newline-separated diagnostics as errors.Join.
func (self *ErrorCauseBatch) Error() string {
	var text strings.Builder
	for index, cause := range self.causes {
		if index != 0 {
			text.WriteByte('\n')
		}
		if cause != nil {
			text.WriteString(cause.Error())
		}
	}
	return text.String()
}

// Callers can inspect every original typed cause without mutating the receipt.
func (self *ErrorCauseBatch) Unwrap() []error {
	return append([]error(nil), self.causes...)
}

// Inspect one explicit batch within an otherwise ordinary bounded graph.
// The outer graph and each member retain the 128-node/32-edge limits. At most
// 512 members are examined, without recursion into another batch. No truncated
// graph grants completeness; every original member is re-read at use time.
func InspectErrorCauseBatch(err error) (result ErrorCauseInspection) {
	defer func() {
		if recover() != nil {
			result.Complete = false
		}
	}()
	result = inspectErrorCauses(err, true)
	if !result.Complete {
		return
	}
	batchIndex := -1
	var batch *ErrorCauseBatch
	for index, node := range result.Nodes {
		if candidate, ok := node.Err.(*ErrorCauseBatch); ok {
			if batch != nil || candidate == nil {
				result.Complete = false
				return
			}
			batchIndex, batch = index, candidate
		}
	}
	if batch == nil {
		return
	}
	result.Nodes[batchIndex].Leaf = false
	if len(batch.causes) == 0 || errorCauseBatchMaximumMembers < len(batch.causes) {
		result.Complete = false
		return
	}
	for _, cause := range batch.causes {
		if cause == nil {
			result.NilBranches++
			result.Complete = false
			continue
		}
		member := InspectErrorCauses(cause)
		result.Complete = result.Complete && member.Complete && member.NilBranches == 0
		result.NilBranches += member.NilBranches
		offset := len(result.Nodes)
		for _, node := range member.Nodes {
			if _, nested := node.Err.(*ErrorCauseBatch); nested {
				result.Complete = false
			}
			if node.Parent == -1 {
				node.Parent = batchIndex
			} else {
				node.Parent += offset
			}
			result.Nodes = append(result.Nodes, node)
		}
	}
	return
}
