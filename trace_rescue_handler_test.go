// HandleError runs every rescue handler it is given, whatever the name of its
// function type, and reports a handler it cannot run instead of skipping it.
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Locally named rescue handler types, which an any keeps distinct from the
// unnamed function types.
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

// Any named type whose underlying type is func() or func(error) runs, in
// order with the unnamed handlers, and none of them is reported.
func TestHandleErrorRunsNamedFunctionTypes(t *testing.T) {
	reports := captureUnexpectedErrorReports(t)
	raise := errors.New("synthetic worker fault")
	order := []string{}
	var received error
	HandleError(
		func() {
			panic(raise)
		},
		testRescueFunc(func() { order = append(order, "named") }),
		func() { order = append(order, "unnamed") },
		testRescueErrorFunc(func(err error) {
			order = append(order, "named error")
			received = err
		}),
	)
	if got := strings.Join(order, ","); got != "named,unnamed,named error" {
		t.Fatalf("handler order=%s, want named,unnamed,named error", got)
	}
	if received != raise {
		t.Fatalf("named error handler received %v, want the recovered raise", received)
	}
	if count := unsupportedRescueHandlerReportCount(*reports); count != 0 {
		t.Fatalf("supported handlers reported %d times", count)
	}
}

// A handler HandleError cannot run is reported once as an unexpected error,
// with its type, and the handlers after it still run. A nil handler is no
// handler and is not reported.
func TestHandleErrorReportsUnsupportedHandler(t *testing.T) {
	cases := []struct {
		name     string
		handler  any
		reported bool
	}{
		{name: "result", handler: func() error { return nil }, reported: true},
		{name: "parameter", handler: func(string) {}, reported: true},
		{name: "named result", handler: testRescueResultFunc(func(error) int { return 0 }), reported: true},
		{name: "string", handler: "synthetic handler", reported: true},
		{name: "nil", handler: nil},
	}
	reports := captureUnexpectedErrorReports(t)
	for _, c := range cases {
		*reports = (*reports)[:0]
		laterRan := false
		HandleError(func() {
			panic(errors.New("synthetic worker fault"))
		}, c.handler, func() {
			laterRan = true
		})
		if !laterRan {
			t.Errorf("%s: the handler after it did not run", c.name)
		}
		count := unsupportedRescueHandlerReportCount(*reports)
		if c.reported && (count != 1 || !strings.Contains(strings.Join(*reports, ""), fmt.Sprintf("%T", c.handler))) {
			t.Errorf("%s: reported %d times, want once with type %T", c.name, count, c.handler)
		}
		if !c.reported && count != 0 {
			t.Errorf("%s: reported %d times, want none", c.name, count)
		}
	}
}

// A named fallback type produces HandleError1's result after a raise.
func TestHandleError1RunsNamedFallback(t *testing.T) {
	raise := errors.New("synthetic lookup fault")
	var received error
	result := HandleError1(func() int {
		panic(raise)
	}, testRescueResultFunc(func(err error) int {
		received = err
		return 7
	}))
	if result != 7 || received != raise {
		t.Fatalf("result=%d received=%v, want 7 and the recovered raise", result, received)
	}
}

// A named fallback type produces HandleError2's results after a raise.
func TestHandleError2RunsNamedFallback(t *testing.T) {
	fallbackErr := errors.New("synthetic fallback")
	result, err := HandleError2(func() (int, error) {
		panic(errors.New("synthetic lookup fault"))
	}, testRescueResultPairFunc(func() (int, error) {
		return 7, fallbackErr
	}))
	if result != 7 || err != fallbackErr {
		t.Fatalf("results=(%d, %v), want (7, the fallback error)", result, err)
	}
}

// A fallback of the wrong result type is reported, and the result stays zero
// unless a later handler produces one.
func TestHandleError1ReportsUnsupportedHandler(t *testing.T) {
	reports := captureUnexpectedErrorReports(t)
	result := HandleError1(func() int {
		panic(errors.New("synthetic lookup fault"))
	}, func(error) string { return "wrong result type" }, func(error) int { return 7 })
	if result != 7 {
		t.Fatalf("result=%d, want 7 from the supported handler", result)
	}
	if count := unsupportedRescueHandlerReportCount(*reports); count != 1 {
		t.Fatalf("unsupported fallback reported %d times, want once", count)
	}
}
