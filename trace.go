package server

import (
	"context"
	"errors"
	// "sync"
	"time"
	// "slices"
	// "os"
	// "os/signal"
	// "syscall"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime/debug"
	"strings"
	// mathrand "math/rand"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"
)

// Only explicit shutdown signals are benign; diagnostic text is not evidence
// of cancellation, and deadlines still require an unexpected-error report.
func IsDoneError(r any) bool {
	switch v := r.(type) {
	case error:
		if joined, ok := v.(interface{ Unwrap() []error }); ok {
			causes := joined.Unwrap()
			if len(causes) == 0 {
				return false
			}
			for _, cause := range causes {
				if !IsDoneError(cause) {
					return false
				}
			}
			return true
		}
		if cause := errors.Unwrap(v); cause != nil {
			return IsDoneError(cause)
		}
		// Connect's client and sequence shutdown paths use both spellings.
		return errors.Is(v, context.Canceled) || errors.Is(v, DbContextDoneError) || v.Error() == "Done" || v.Error() == "Done."
	case string:
		return v == "Done" || v == "Done."
	default:
		return false
	}
}

// A refusal under ordinary contention, such as a business owner another
// writer holds. Its raiser expects it and leaves the work to the holder or a
// later attempt, so HandleError counts it instead of reporting an unexpected
// error with a stack.
type ContentionError struct {
	message string
}

// A refusal identity; callers compare it with errors.Is.
func NewContentionError(message string) *ContentionError {
	return &ContentionError{message: message}
}

// The refusal's message, unchanged from a plain error's.
func (self *ContentionError) Error() string {
	return self.message
}

// Every concrete leaf, through wrapping and joins, is a contention refusal.
// Incomplete or malformed cause graphs do not classify as contention.
func IsContentionError(r any) bool {
	err, ok := r.(error)
	if !ok || err == nil {
		return false
	}
	causes := InspectErrorCauseBatch(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	found := false
	for _, cause := range causes.Nodes {
		if !cause.Leaf {
			continue
		}
		if contention, ok := cause.Err.(*ContentionError); !ok || contention == nil {
			return false
		}
		found = true
	}
	return found
}

var contentionRecoveredCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Name:      "contention_refusals_recovered_total",
	Help:      "Contention refusals, such as a held balance owner, recovered by HandleError without an unexpected-error report",
})

// Registers the contention counter with the default registry.
func init() {
	prometheus.MustRegister(contentionRecoveredCounter)
}

// The unexpected-error report; a test observes it.
var unexpectedErrorLogf = glog.Infof

// The report for a rescue handler that cannot run, which is a caller bug. It
// reads only the handler's type: giving the handler itself to fmt would make
// every caller's handlers escape to the heap, raise or not.
func unsupportedRescueHandlerError(handler any, supported string) error {
	return fmt.Errorf("rescue handler of type %v was not run: rescue handlers here are %s", reflect.TypeOf(handler), supported)
}

// Recovers a raise, reports it unless it is a shutdown or a contention
// refusal, and runs each rescue handler in order. A handler is a func(), a
// func(error) given the raise as an error, a context.CancelFunc, or a
// context.CancelCauseFunc given the raise as its cause. A value keeps its
// named type in an any, so the two context cancels have cases of their own;
// any other handler, another named function type included, cannot run and is
// reported as an unexpected error. A nil handler is skipped.
//
// Handlers run on every raise, a cancellation raise included. A cancel
// handler therefore ends its owner whenever the work raises, even when the
// raise is the cancellation of a narrower context the work was given: pass
// only the cancel of an owner whose life should end with this work. A handler
// that panics is contained, and the remaining handlers still run.
//
// Neither do nor the handlers escape, so a caller's closures stay on its
// stack. Keep it that way: a handler given to fmt or reflect.ValueOf escapes.
func HandleError(do func(), handlers ...any) (r any) {
	defer func() {
		if r = recover(); r != nil {
			if IsDoneError(r) {
				// the context was canceled and raised. this is a standard pattern, do not log
			} else if IsContentionError(r) {
				// expected backpressure: count it, without a stack
				contentionRecoveredCounter.Inc()
			} else {
				unexpectedErrorLogf("Unexpected error: %s\n", ErrorJson(r, debug.Stack()))
			}
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("%s", r)
			}
			for _, handler := range handlers {
				// a rescue handler that panics (e.g. a double-registered
				// wg.Done) must not escalate the recovery into a process
				// crash; contain it and log loudly, then run the remaining
				// handlers (mirrors connect.HandleError)
				func() {
					defer func() {
						if handlerR := recover(); handlerR != nil {
							glog.Infof("Rescue handler panic (contained): %s\n", ErrorJson(handlerR, debug.Stack()))
						}
					}()
					switch v := handler.(type) {
					case nil:
					case func():
						v()
					case func(error):
						v(err)
					case context.CancelFunc:
						v()
					case context.CancelCauseFunc:
						v(err)
					default:
						unexpectedErrorLogf("Unexpected error: %s\n", ErrorJson(unsupportedRescueHandlerError(handler, "func(), func(error), context.CancelFunc and context.CancelCauseFunc"), debug.Stack()))
					}
				}()
			}
		}
	}()
	do()
	return
}

// Returns do's result, or after a raise the result of the last rescue handler
// that produced one. A handler is a func() R, or a func(error) R given the
// raise as an error. Any other handler, a named function type included, is
// reported as an unexpected error, and a nil handler is skipped.
func HandleError1[R any](do func() R, handlers ...any) (result R) {
	HandleError(func() {
		result = do()
	}, func(err error) {
		for _, handler := range handlers {
			switch v := handler.(type) {
			case nil:
			case func() R:
				result = v()
			case func(error) R:
				result = v(err)
			default:
				unexpectedErrorLogf("Unexpected error: %s\n", ErrorJson(unsupportedRescueHandlerError(handler, "func() and func(error) returning the helper's result"), debug.Stack()))
			}
		}
	})
	return
}

// HandleError1 for a pair of results: handlers are func() (R1, R2) and
// func(error) (R1, R2).
func HandleError2[R1 any, R2 any](do func() (R1, R2), handlers ...any) (result1 R1, result2 R2) {
	HandleError(func() {
		result1, result2 = do()
	}, func(err error) {
		for _, handler := range handlers {
			switch v := handler.(type) {
			case nil:
			case func() (R1, R2):
				result1, result2 = v()
			case func(error) (R1, R2):
				result1, result2 = v(err)
			default:
				unexpectedErrorLogf("Unexpected error: %s\n", ErrorJson(unsupportedRescueHandlerError(handler, "func() and func(error) returning the helper's results"), debug.Stack()))
			}
		}
	})
	return
}

func HandleErrorWithReturn[R any](do func() R) (result R) {
	return HandleError1(do)
}

func ErrorJson(err any, stack []byte) string {
	stackLines := []string{}
	for _, line := range strings.Split(string(stack), "\n") {
		stackLines = append(stackLines, strings.TrimSpace(line))
	}
	errorJson, _ := json.Marshal(map[string]any{
		"error": fmt.Sprintf("%T=%s", err, err),
		"stack": stackLines,
	})
	return string(errorJson)
}

func ErrorJsonNoStack(err any) string {
	errorJson, _ := json.Marshal(map[string]any{
		"error": fmt.Sprintf("%T=%s", err, err),
	})
	return string(errorJson)
}

func ErrorJsonWithCustomNoStack(err any, custom map[string]any) string {
	obj := map[string]any{
		"error": fmt.Sprintf("%T=%s", err, err),
	}
	for key, value := range custom {
		obj[key] = SafeLogValue(value)
	}
	errorJson, _ := json.Marshal(obj)
	return string(errorJson)
}

func Trace(tag string, do func()) {
	trace(tag, func() string {
		do()
		return ""
	})
}

func TraceWithReturn[R any](tag string, do func() R) (result R) {
	trace(tag, func() string {
		result = do()
		return fmt.Sprintf(" = %v", result)
	})
	return
}

func TraceWithReturnShallowLog[R any](tag string, do func() R) (result R) {
	trace(tag, func() string {
		result = do()
		return fmt.Sprintf(" = %T", result)
	})
	return
}

func trace(tag string, do func() string) {
	start := time.Now()
	glog.Infof("[%-8s]%s (%d)\n", "start", tag, start.UnixMilli())
	doTag := do()
	end := time.Now()
	millis := float32(end.Sub(start)) / float32(time.Millisecond)
	glog.Infof("[%-8s]%s (%.2fms) (%d)%s\n", "end", tag, millis, end.UnixMilli(), doTag)
}
