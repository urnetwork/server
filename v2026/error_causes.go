// Cause inspection has a finite work and depth budget. It does not invoke
// custom Is/As methods or decide retry, absence, authority, or cancellation.
package server

import (
	"net"
	"reflect"
)

const errorCauseMaximumNodes = 128
const errorCauseMaximumDepth = 32

// Parent is an earlier node index, or -1 for the original cause. Leaves have
// no unwrap method; an empty/malformed wrapper never becomes a typed leaf.
type ErrorCauseNode struct {
	Err    error
	Parent int
	Leaf   bool
	depth  int
}

// Incomplete observation grants no positive classification. Nodes already
// observed remain available so explicit hard causes can retain precedence.
type ErrorCauseInspection struct {
	Nodes       []ErrorCauseNode
	Complete    bool
	NilBranches int
}

// At most 128 cause slots (including nil) and 32 unwrap edges are examined.
// Breadth-first inspection retains nearby independent hard causes even when
// another branch cycles. Each caller owns its separate leaf/ancestry policy.
func InspectErrorCauses(err error) ErrorCauseInspection {
	return inspectErrorCauses(err, false)
}

// Explicit batches can form opaque boundaries only for the separate batch
// inspector, which must then inspect every original member independently.
func inspectErrorCauses(err error, stopAtBatch bool) ErrorCauseInspection {
	if err == nil {
		return ErrorCauseInspection{}
	}
	result := ErrorCauseInspection{Nodes: []ErrorCauseNode{{Err: err, Parent: -1}}, Complete: true}
	work := 1
	for index := 0; index < len(result.Nodes); index++ {
		node := result.Nodes[index]
		// A typed nil is neither an observation nor permission to call an
		// unwrap method on a missing receiver.
		value := reflect.ValueOf(node.Err)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				result.Complete = false
				continue
			}
		}
		if _, batch := node.Err.(*ErrorCauseBatch); stopAtBatch && batch {
			result.Nodes[index].Leaf = true
			continue
		}
		var causes []error
		switch value := node.Err.(type) {
		case interface{ Unwrap() []error }:
			if node.depth >= errorCauseMaximumDepth {
				result.Complete = false
				continue
			}
			causes = value.Unwrap()
		case interface{ Unwrap() error }:
			if node.depth >= errorCauseMaximumDepth {
				result.Complete = false
				continue
			}
			cause := value.Unwrap()
			// The standard DNS error carries its own typed outcome and has
			// an optional UnwrapErr. Its missing nested cause is a real leaf.
			if _, dns := node.Err.(*net.DNSError); dns && cause == nil {
				result.Nodes[index].Leaf = true
				continue
			}
			causes = []error{cause}
		default:
			result.Nodes[index].Leaf = true
			continue
		}
		if len(causes) == 0 {
			result.Complete = false
			continue
		}
		if len(causes) > errorCauseMaximumNodes-work {
			result.Complete = false
			causes = causes[:errorCauseMaximumNodes-work]
		}
		found := false
		for _, cause := range causes {
			work++
			if cause == nil {
				result.NilBranches++
				continue
			}
			found = true
			result.Nodes = append(result.Nodes, ErrorCauseNode{Err: cause, Parent: index, depth: node.depth + 1})
		}
		if !found {
			result.Complete = false
		}
	}
	return result
}
