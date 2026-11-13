// Shared read-only validation authenticates the configured native main integration and the exact managed competition route without claiming notification delivery.
package grafana

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

// Each inspection owns its response and entry-count bounds; non-positive values are unsupported.
type competitionRoutingValidationSettings struct {
	MaximumJsonBytes    int
	MaximumContacts     int
	MaximumRoutes       int
	MaximumMatchers     int
	MaximumMatcherBytes int
}

// Bounds match the existing provisioning response envelope and limit pure parsing work.
func defaultCompetitionRoutingValidationSettings() competitionRoutingValidationSettings {
	return competitionRoutingValidationSettings{MaximumJsonBytes: 1024 * 1024, MaximumContacts: 512, MaximumRoutes: 512, MaximumMatchers: 64, MaximumMatcherBytes: 4096}
}

// The same canonical ownership predicate used by reconciliation protects preflight from unrelated receiver names or manual route text.
func ValidateCompetitionAlertRouting(contactsJson []byte, policyJson []byte) error {
	settings := defaultCompetitionRoutingValidationSettings()
	if len(contactsJson) == 0 || len(policyJson) == 0 || len(contactsJson) > settings.MaximumJsonBytes || len(policyJson) > settings.MaximumJsonBytes {
		return errors.New("Grafana routing response is empty or exceeds its bound")
	}
	var contacts []grafanaContactPoint
	if json.Unmarshal(contactsJson, &contacts) != nil || len(contacts) > settings.MaximumContacts || !configuredDefaultAlertContact(contacts) {
		return errors.New("Grafana existing main Slack integration is not configured")
	}
	policy := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(policyJson))
	decoder.UseNumber()
	if decoder.Decode(&policy) != nil {
		return errors.New("Grafana notification policy is malformed")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("Grafana notification policy has trailing JSON")
	}
	rootReceiver, ok := policy["receiver"].(string)
	if !ok || rootReceiver == "" {
		return errors.New("Grafana notification policy root receiver is missing")
	}
	routes, ok := policy["routes"].([]any)
	if !ok || len(routes) > settings.MaximumRoutes {
		return errors.New("Grafana competition routes are absent or exceed their bound")
	}
	canonical := 0
	canonicalIndex := -1
	for index, value := range routes {
		if !managedCompetitionRoute(value) {
			continue
		}
		route := value.(map[string]any)
		if route["receiver"] != DefaultAlertContactName {
			return errors.New("Grafana canonical competition route still targets the retired email transport")
		}
		canonical++
		canonicalIndex = index
	}
	if canonical != 1 {
		return errors.New("Grafana must contain exactly one canonical competition route to the existing main receiver")
	}
	for _, prior := range routes[:canonicalIndex] {
		blocks, err := competitionRouteBlocksCanonical(prior, settings)
		if err != nil {
			return err
		}
		if blocks {
			return errors.New("Grafana canonical competition route is shadowed by an earlier or ambiguous manual route")
		}
	}
	return nil
}

// First-match siblings can block either required severity. Only known service/severity values or a typed continuation prove otherwise; unknown labels remain unconstrained.
func competitionRouteBlocksCanonical(value any, settings competitionRoutingValidationSettings) (bool, error) {
	route, ok := value.(map[string]any)
	if !ok {
		return true, errors.New("Grafana prior route is malformed")
	}
	if receiver, exists := route["receiver"]; exists {
		if _, ok := receiver.(string); !ok {
			return true, errors.New("Grafana prior receiver is malformed")
		}
	}
	continues := false
	if value, exists := route["continue"]; exists {
		var valid bool
		continues, valid = value.(bool)
		if !valid {
			return true, errors.New("Grafana prior continuation is malformed")
		}
	}
	for _, legacy := range []string{"match", "match_re", "matchers"} {
		if _, exists := route[legacy]; exists {
			return true, errors.New("Grafana prior matcher schema cannot prove competition reachability")
		}
	}
	if nested, exists := route["routes"]; exists {
		children, ok := nested.([]any)
		if !ok || len(children) > settings.MaximumRoutes {
			return true, errors.New("Grafana prior child routes are malformed or exceed their bound")
		}
	}
	matchers := []any{}
	if value, exists := route["object_matchers"]; exists {
		var valid bool
		matchers, valid = value.([]any)
		if !valid {
			return true, errors.New("Grafana prior matchers are malformed")
		}
	}
	if len(matchers) > settings.MaximumMatchers {
		return true, errors.New("Grafana prior matchers exceed their bound")
	}
	mayWarn, mayPage := true, true
	for _, value := range matchers {
		matcher, ok := value.([]any)
		if !ok || len(matcher) != 3 {
			return true, errors.New("Grafana prior matcher tuple is malformed")
		}
		name, nameOk := matcher[0].(string)
		operator, operatorOk := matcher[1].(string)
		pattern, patternOk := matcher[2].(string)
		if !nameOk || !operatorOk || !patternOk || name == "" || len(name) > settings.MaximumMatcherBytes || len(pattern) > settings.MaximumMatcherBytes {
			return true, errors.New("Grafana prior matcher fields are malformed or exceed their bound")
		}
		var compiled *regexp.Regexp
		switch operator {
		case "=", "!=":
		case "=~", "!~":
			var err error
			compiled, err = regexp.Compile("^(?:" + pattern + ")$")
			if err != nil {
				return true, errors.New("Grafana prior matcher regex is malformed")
			}
		default:
			return true, errors.New("Grafana prior matcher operator cannot prove competition reachability")
		}
		matches := func(actual string) bool {
			switch operator {
			case "=":
				return actual == pattern
			case "!=":
				return actual != pattern
			case "=~":
				return compiled.MatchString(actual)
			default:
				return !compiled.MatchString(actual)
			}
		}
		switch name {
		case "service":
			allowed := matches("sim-latency")
			mayWarn = mayWarn && allowed
			mayPage = mayPage && allowed
		case "severity":
			mayWarn = mayWarn && matches("warn")
			mayPage = mayPage && matches("page")
		}
	}
	return !continues && (mayWarn || mayPage), nil
}
