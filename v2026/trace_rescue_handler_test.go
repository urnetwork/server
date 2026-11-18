// HandleError runs every rescue handler it can, the two named context cancels
// included, reports a handler it cannot run instead of skipping it, and costs
// its callers nothing until a raise.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Locally named rescue handler types, which an any keeps distinct from the
// unnamed function types. HandleError runs none of them.
type testRescueFunc func()
type testRescueErrorFunc func(error)
type testRescueResultFunc func(error) int
type testRescueResultPairFunc func() (int, error)

// Captures the unexpected-error reports of the calling test.
func captureUnexpectedErrorReports(t *testing.T) *[]string {
	t.Helper()
	reports := []string{}
	logf := unexpectedErrorLogf
	unexpectedErrorLogf = func(format string, args ...any) {
		reports = append(reports, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { unexpectedErrorLogf = logf })
	return &reports
}

// Counts the reports of a rescue handler that was not run.
func unsupportedRescueHandlerReportCount(reports []string) int {
	count := 0
	for _, report := range reports {
		if strings.Contains(report, "was not run") {
			count += 1
		}
	}
	return count
}

// A context.CancelFunc keeps its named type when passed through any, so a
// switch on func() alone skipped it and the context stayed live.
func TestHandleErrorRunsNamedContextCancelFunc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	HandleError(func() {
		panic(errors.New("synthetic worker fault"))
	}, cancel)
	if ctx.Err() == nil {
		t.Fatal("recovery did not run the context.CancelFunc handler")
	}
}

// A context.CancelCauseFunc receives the recovered raise as its cause.
func TestHandleErrorRunsNamedContextCancelCauseFunc(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	raise := errors.New("synthetic worker fault")
	HandleError(func() {
		panic(raise)
	}, cancel)
	if cause := context.Cause(ctx); cause != raise {
		t.Fatalf("cancellation cause=%v, want the recovered raise", cause)
	}
}

// The handlers run in order, the context cancel among them, and none of them
// is reported.
func TestHandleErrorRunsHandlersInOrder(t *testing.T) {
	reports := captureUnexpectedErrorReports(t)
	raise := errors.New("synthetic worker fault")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	order := []string{}
	var received error
	HandleError(
		func() {
			panic(raise)
		},
		func() { order = append(order, "func") },
		cancel,
		func(err error) {
			if ctx.Err() != nil {
				order = append(order, "after cancel")
			}
			received = err
		},
	)
	if got := strings.Join(order, ","); got != "func,after cancel" {
		t.Fatalf("handler order=%s, want func,after cancel", got)
	}
	if received != raise {
		t.Fatalf("error handler received %v, want the recovered raise", received)
	}
	if count := unsupportedRescueHandlerReportCount(*reports); count != 0 {
		t.Fatalf("supported handlers reported %d times", count)
	}
}

// A handler HandleError cannot run, another named function type included, is
// not run and is reported once as an unexpected error with its type, and the
// handlers after it still run. A nil handler is no handler and is not
// reported.
func TestHandleErrorReportsUnsupportedHandler(t *testing.T) {
	cases := []struct {
		name     string
		handler  func(ran *bool) any
		reported bool
	}{
		{name: "result", handler: func(ran *bool) any { return func() error { *ran = true; return nil } }, reported: true},
		{name: "parameter", handler: func(ran *bool) any { return func(string) { *ran = true } }, reported: true},
		{name: "named", handler: func(ran *bool) any { return testRescueFunc(func() { *ran = true }) }, reported: true},
		{name: "named error", handler: func(ran *bool) any { return testRescueErrorFunc(func(error) { *ran = true }) }, reported: true},
		{name: "named result", handler: func(ran *bool) any { return testRescueResultFunc(func(error) int { *ran = true; return 0 }) }, reported: true},
		{name: "string", handler: func(*bool) any { return "synthetic handler" }, reported: true},
		{name: "nil", handler: func(*bool) any { return nil }},
	}
	reports := captureUnexpectedErrorReports(t)
	for _, c := range cases {
		*reports = (*reports)[:0]
		ran := false
		handler := c.handler(&ran)
		laterRan := false
		HandleError(func() {
			panic(errors.New("synthetic worker fault"))
		}, handler, func() {
			laterRan = true
		})
		if ran {
			t.Errorf("%s: the unsupported handler ran", c.name)
		}
		if !laterRan {
			t.Errorf("%s: the handler after it did not run", c.name)
		}
		count := unsupportedRescueHandlerReportCount(*reports)
		if c.reported && (count != 1 || !strings.Contains(strings.Join(*reports, ""), fmt.Sprintf("%T", handler))) {
			t.Errorf("%s: reported %d times, want once with type %T", c.name, count, handler)
		}
		if !c.reported && count != 0 {
			t.Errorf("%s: reported %d times, want none", c.name, count)
		}
	}
}

// A fallback of the wrong result type, or of a named function type, is not run
// and is reported, and a later supported handler produces the result.
func TestHandleError1ReportsUnsupportedHandler(t *testing.T) {
	cases := []struct {
		name    string
		handler func(ran *bool) any
	}{
		{name: "result type", handler: func(ran *bool) any { return func(error) string { *ran = true; return "" } }},
		{name: "named", handler: func(ran *bool) any { return testRescueResultFunc(func(error) int { *ran = true; return 3 }) }},
	}
	reports := captureUnexpectedErrorReports(t)
	for _, c := range cases {
		*reports = (*reports)[:0]
		ran := false
		result := HandleError1(func() int {
			panic(errors.New("synthetic lookup fault"))
		}, c.handler(&ran), func(error) int { return 7 })
		if ran || result != 7 {
			t.Errorf("%s: ran=%t result=%d, want the unsupported fallback skipped and 7", c.name, ran, result)
		}
		if count := unsupportedRescueHandlerReportCount(*reports); count != 1 {
			t.Errorf("%s: reported %d times, want once", c.name, count)
		}
	}
}

// A named fallback type is not run by HandleError2 and is reported, and a
// later supported handler produces the results.
func TestHandleError2ReportsUnsupportedHandler(t *testing.T) {
	reports := captureUnexpectedErrorReports(t)
	fallbackErr := errors.New("synthetic fallback")
	ran := false
	result, err := HandleError2(func() (int, error) {
		panic(errors.New("synthetic lookup fault"))
	}, testRescueResultPairFunc(func() (int, error) {
		ran = true
		return 3, nil
	}), func() (int, error) {
		return 7, fallbackErr
	})
	if ran || result != 7 || err != fallbackErr {
		t.Fatalf("ran=%t results=(%d, %v), want the named fallback skipped and (7, the fallback error)", ran, result, err)
	}
	if count := unsupportedRescueHandlerReportCount(*reports); count != 1 {
		t.Fatalf("named fallback reported %d times, want once", count)
	}
}

// Handlers cost nothing until a raise. HandleError wraps hot work such as
// per-connection workers, so if its handlers escaped, every caller's handler
// closure would move to the heap on each call, and so would the result and
// wrapper closure of HandleError1 and HandleError2. Each call here captures a
// variable, so an escaping closure allocates.
func TestHandleErrorAllocatesNothingWithoutARaise(t *testing.T) {
	count := 0
	cases := []struct {
		name string
		call func()
	}{
		{name: "HandleError", call: func() {
			HandleError(func() { count += 1 }, func() { count -= 1 }, func(error) { count -= 1 })
		}},
		{name: "HandleError1", call: func() {
			count += HandleError1(func() int { return count }, func(error) int { return -count })
		}},
		{name: "HandleError2", call: func() {
			result, _ := HandleError2(func() (int, error) { return count, nil }, func(error) (int, error) { return -count, nil })
			count += result
		}},
	}
	for _, c := range cases {
		if allocs := testing.AllocsPerRun(100, c.call); allocs != 0 {
			t.Errorf("%s: %v allocations per call without a raise, want 0", c.name, allocs)
		}
	}
}
