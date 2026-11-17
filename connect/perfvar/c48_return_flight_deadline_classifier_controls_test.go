// Deadline controls retain extra payload/owner causes as hard failures and
// exercise the exact classifier used by the private performance report.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

// Ordinary HTTP/fmt wrapping of one deadline remains a numeric workload miss.
func TestC48ReturnFlightDeadlineAllowsWrappedSingleCause(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded,
		fmt.Errorf("page 7 HTML: %w", &url.Error{Op: "Get", URL: "https://origin.example/page/7", Err: context.DeadlineExceeded}),
		errors.Join(fmt.Errorf("one deadline: %w", context.DeadlineExceeded), nil),
	} {
		if !c48ReturnFlightDeadlineOnly(err) || c48ReturnFlightWorkloadErrorClass(err) != "deadline" || c48ReturnFlightWorkloadOwnershipFailed(err) {
			t.Fatalf("one deadline was reclassified: %v", err)
		}
	}
}

// Either join order keeps response corruption visible ahead of a deadline.
func TestC48ReturnFlightDeadlineRejectsJoinedPayloadMismatch(t *testing.T) {
	for _, err := range []error{
		errors.Join(context.DeadlineExceeded, errNativeBrowsingResponseMismatch),
		errors.Join(errNativeBrowsingResponseMismatch, context.DeadlineExceeded),
	} {
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errNativeBrowsingResponseMismatch) ||
			c48ReturnFlightDeadlineOnly(err) || c48ReturnFlightWorkloadErrorClass(err) != "response-validation" {
			t.Fatalf("deadline concealed payload corruption: %v", err)
		}
	}
}

// A failed deferred owner join keeps its exact error text and sentinel chain,
// including when the cleanup failure itself wraps only a deadline.
func TestC48ReturnFlightDeadlineRejectsMarkedOwnershipFailure(t *testing.T) {
	for _, cause := range []error{
		errors.Join(context.DeadlineExceeded, errors.New("synthetic browsing owners did not join")),
		errors.Join(fmt.Errorf("synthetic browsing handler shutdown: %w", context.DeadlineExceeded), nil),
		errors.Join(context.DeadlineExceeded, errNativeBrowsingResponseMismatch),
	} {
		err := &c48ReturnFlightWorkloadOwnerFailure{cause: cause}
		wantClass := "other"
		if errors.Is(cause, errNativeBrowsingResponseMismatch) {
			wantClass = "response-validation"
		}
		if err.Error() != cause.Error() || !errors.Is(err, context.DeadlineExceeded) ||
			!c48ReturnFlightWorkloadOwnershipFailed(err) || c48ReturnFlightDeadlineOnly(err) ||
			c48ReturnFlightWorkloadErrorClass(err) != wantClass {
			t.Fatalf("deadline concealed owner cleanup or changed its error: %v", err)
		}
		if errors.Is(cause, errNativeBrowsingResponseMismatch) && !errors.Is(err, errNativeBrowsingResponseMismatch) {
			t.Fatal("owner marker concealed payload mismatch")
		}
	}
}

// An additional cause cannot acquire deadline-only authority, even if every
// leaf happens to be a deadline; nil and unknown errors are not misses either.
func TestC48ReturnFlightDeadlineRejectsAdditionalCauses(t *testing.T) {
	for _, err := range []error{
		errors.Join(context.DeadlineExceeded, errors.New("unclassified fixture failure")),
		errors.Join(context.DeadlineExceeded, context.Canceled),
		errors.Join(context.DeadlineExceeded, context.DeadlineExceeded),
		fmt.Errorf("wrapped join: %w", errors.Join(context.DeadlineExceeded, errors.New("other failure"))),
		nil,
		errors.New("no deadline evidence"),
	} {
		if c48ReturnFlightDeadlineOnly(err) || c48ReturnFlightWorkloadErrorClass(err) == "deadline" {
			t.Fatalf("additional or missing cause became a numeric deadline: %v", err)
		}
	}
}
