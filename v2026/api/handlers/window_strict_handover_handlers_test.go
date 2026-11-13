package handlers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

type windowStrictHandoverTestOverride struct {
	enabled bool
	set     bool
}

func (self windowStrictHandoverTestOverride) Override() (bool, bool) {
	return self.enabled, self.set
}

// Every find-providers2 answer the http route serves carries the platform's
// override of the apps' strict window handover when there is one, on a copy:
// the model's answer is never changed, an answer without an override is the
// model's answer as it is, and an error is passed through.
func TestFindProviders2CarriesTheWindowStrictHandoverOverride(t *testing.T) {
	answer := &model.FindProviders2Result{Providers: []*model.FindProvidersProvider{}}
	var findErr error
	find := func(args *model.FindProviders2Args, clientSession *session.ClientSession) (*model.FindProviders2Result, error) {
		if findErr != nil {
			return nil, findErr
		}
		return answer, nil
	}
	encode := func(result *model.FindProviders2Result) string {
		t.Helper()
		body, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	for _, c := range []struct {
		name     string
		override windowStrictHandoverOverride
		body     string
	}{
		{"off", windowStrictHandoverTestOverride{enabled: false, set: true}, `{"providers":[],"window_strict_handover":false}`},
		{"on", windowStrictHandoverTestOverride{enabled: true, set: true}, `{"providers":[],"window_strict_handover":true}`},
		{"none", windowStrictHandoverTestOverride{}, `{"providers":[]}`},
		{"no owner", (*controller.WindowStrictHandoverControl)(nil), `{"providers":[]}`},
	} {
		result, err := findProviders2WithWindowStrictHandover(find, c.override)(&model.FindProviders2Args{}, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if body := encode(result); body != c.body {
			t.Errorf("%s: answered %s, want %s", c.name, body, c.body)
		}
		if answer.WindowStrictHandover != nil {
			t.Fatalf("%s: the model's answer was changed", c.name)
		}
	}

	findErr = errors.New("find failed")
	result, err := findProviders2WithWindowStrictHandover(find, windowStrictHandoverTestOverride{set: true})(&model.FindProviders2Args{}, nil)
	if err == nil || result != nil || !strings.Contains(err.Error(), "find failed") {
		t.Fatalf("an error answered (%v, %v)", result, err)
	}
}
