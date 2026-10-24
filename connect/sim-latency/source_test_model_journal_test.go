// Compose real journal code with mock provider streams and an observed fixed
// test callback. Restarts must replay facts, not request another judgment/run.
package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Both model and execution journals bind the same private image/owner plan.
func journaledSourceTestModelFixture(t *testing.T) (sourceTestJournalFixture, sourceTestModelSettings) {
	t.Helper()
	fixture := newSourceTestJournalFixture(t)
	settings := defaultSourceTestModelSettings()
	settings.ResponseJournalDirectory = filepath.Join(filepath.Dir(fixture.Directory), "independent-model-journal")
	return fixture, settings
}

func TestSourceTestModelJournalReplaysProviderAndExecutionWithoutAnotherDraw(t *testing.T) {
	fixture, settings := journaledSourceTestModelFixture(t)
	requests, executions := 0, 0
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		requests++
		stream := sourceTestToolStream(t, "run_prescribed_tests", "{}", false)
		if requests == 2 {
			stream = winnerModelFixtureStream(t, `{"passed":true,"summary":"Synthetic prescribed tests passed."}`)
		} else if requests != 1 {
			t.Fatal("restart sampled another provider response")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(stream)), Header: make(http.Header)}, nil
	})}
	run := func(ctx context.Context) (sourceTestExecution, error) {
		result, err := executeJournaledSourceTest(ctx, fixture.Directory, fixture.Plan, fixture.Now.Add(time.Hour), fixture.Settings,
			func(context.Context) (*sourceTestExecution, error) {
				executions++
				value := fixture.Execution
				return &value, nil
			}, fixture.verify)
		if err != nil {
			return sourceTestExecution{}, err
		}
		return *result, nil
	}
	first, err := executeSourceTestModel(context.Background(), client, "synthetic-model-key", fixture.Plan, settings, run)
	if err != nil {
		t.Fatal(err)
	}
	second, err := executeSourceTestModel(context.Background(), client, "synthetic-rotated-model-key", fixture.Plan, settings, run)
	if err != nil || !reflect.DeepEqual(first, second) || requests != 2 || executions != 1 {
		t.Fatal("model/execution journal failed to replay exact evidence", requests, executions, err)
	}
	for _, phase := range []string{"invocation", "completion"} {
		content, err := os.ReadFile(filepath.Join(settings.ResponseJournalDirectory, phase+"-observation.json"))
		if err != nil || strings.Contains(string(content), "synthetic-model-key") || strings.Contains(string(content), "synthetic-rotated-model-key") {
			t.Fatal("credential entered provider journal", err)
		}
	}
}

func TestSourceTestModelJournalRetainsMalformedCompletedToolBeforeValidation(t *testing.T) {
	fixture, settings := journaledSourceTestModelFixture(t)
	requests := 0
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(sourceTestToolStream(t, "run_shell", "{}", false))), Header: make(http.Header)}, nil
	})}
	for range 2 {
		result, err := executeSourceTestModel(context.Background(), client, "synthetic-model-key", fixture.Plan, settings, func(context.Context) (sourceTestExecution, error) {
			t.Fatal("unsupported completed tool reached real execution")
			return sourceTestExecution{}, nil
		})
		if err == nil || result != nil {
			t.Fatal("unsupported completed provider output became test evidence")
		}
	}
	if requests != 1 {
		t.Fatal("completed malformed tool was resampled", requests)
	}
}

func TestSourceTestModelJournalTransportUncertaintyCannotAutomaticallyRetry(t *testing.T) {
	fixture, settings := journaledSourceTestModelFixture(t)
	requests := 0
	client := &http.Client{Transport: winnerReviewTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("synthetic interrupted provider transport")
	})}
	for range 2 {
		result, err := executeSourceTestModel(context.Background(), client, "synthetic-model-key", fixture.Plan, settings, func(context.Context) (sourceTestExecution, error) {
			t.Fatal("uncertain provider dispatch started prescribed tests")
			return sourceTestExecution{}, nil
		})
		if err == nil || result != nil {
			t.Fatal("transport uncertainty invented provider evidence")
		}
	}
	if requests != 1 {
		t.Fatal("uncertain dispatch silently sampled another request", requests)
	}
}

func TestSourceTestModelJournalChangedPolicyCannotResetOriginalPlan(t *testing.T) {
	fixture, settings := journaledSourceTestModelFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), settings.TotalTimeout)
	defer cancel()
	first, release, err := sourceTestModelJournalContext(ctx, fixture.Plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	original, _ := first.Deadline()
	next, nextCancel, err := sourceTestModelJournalContext(ctx, fixture.Plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer nextCancel()
	retained, _ := next.Deadline()
	if !original.Equal(retained) {
		t.Fatal("replacement model plan extended the original deadline")
	}
	settings.TotalTimeout += time.Hour
	if _, cancel, err := sourceTestModelJournalContext(ctx, fixture.Plan, settings); err == nil {
		cancel()
		t.Fatal("changed model policy reused original authority")
	}
}

func TestSourceTestModelJournalInterruptedDispatchCannotRestartProvider(t *testing.T) {
	fixture, settings := journaledSourceTestModelFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), settings.TotalTimeout)
	defer cancel()
	bounded, release, err := sourceTestModelJournalContext(ctx, fixture.Plan, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("synthetic provider interruption did not occur")
			}
		}()
		journalSourceTestProviderStream(bounded, "invocation", []byte("synthetic-provider-request"), settings, func() ([]byte, error) {
			panic("synthetic crash after dispatch reservation")
		})
	}()
	if _, err := journalSourceTestProviderStream(bounded, "invocation", []byte("synthetic-provider-request"), settings, func() ([]byte, error) {
		t.Fatal("interrupted provider dispatch was repeated")
		return nil, nil
	}); err == nil || !strings.Contains(err.Error(), "active or interrupted") {
		t.Fatal("missing provider observation was treated as an unused slot", err)
	}
}
