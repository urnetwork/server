package monitor

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Severity is the operator-facing urgency of an Alert.
type Severity string

const (
	SeverityPage Severity = "page"
	SeverityWarn Severity = "warn"
)

// Alert is the structured result of a Signal. Fields intentionally mirror the
// actionable-item shape in SIGNALS.md §6b so the same value can be consumed by
// code or rendered as a detailed Markdown issue for a person.
type Alert struct {
	SignalNumber string    `json:"signal_number"`
	SignalKey    string    `json:"signal_key"`
	SignalID     string    `json:"signal_id"`
	SignalName   string    `json:"signal_name"`
	Severity     Severity  `json:"severity"`
	Class        string    `json:"class"`
	Target       string    `json:"target"`
	Frame        string    `json:"frame,omitempty"`
	Environment  string    `json:"environment"`
	ObservedAt   time.Time `json:"observed_at"`
	Sustain      int       `json:"sustain,omitempty"`

	Symptom   string `json:"symptom"`
	Mechanism string `json:"mechanism"`
	Baseline  string `json:"baseline"`
	Observed  string `json:"observed"`
	Evidence  string `json:"evidence,omitempty"`
	Context   string `json:"context,omitempty"`
	Action    string `json:"action"`
	Verify    string `json:"verify"`
	Playbook  string `json:"playbook,omitempty"`
}

// Alerts is the reusable collection returned by signals and monitor runs.
type Alerts []Alert

// Markdown renders this collection as one deterministic alert document.
func (alerts Alerts) Markdown() string { return AlertsMarkdown(alerts) }

// ToMarkdown is the verb-shaped collection conversion alias.
func (alerts Alerts) ToMarkdown() string { return alerts.Markdown() }

// WriteMarkdown writes this collection's deterministic alert document.
func (alerts Alerts) WriteMarkdown(w io.Writer) error { return WriteAlertsMarkdown(w, alerts) }

// Identity is the stable de-duplication key prescribed by SIGNALS.md §7.
// Target is stable; incident-varying attribution belongs in Frame.
func (a Alert) Identity() string {
	return strings.Join([]string{a.SignalID, a.Class, a.Target, a.Frame}, "|")
}

// Markdown renders one alert as a standalone, human-readable Markdown issue.
func (a Alert) Markdown() string {
	var b strings.Builder
	title := firstNonempty(a.Symptom, a.SignalName, a.SignalID, "monitor alert")
	fmt.Fprintf(&b, "## [%s] %s\n\n", strings.ToUpper(string(a.Severity)), markdownLine(title))

	rows := [][2]string{
		{"Signal", signalReference(a)},
		{"Identity", inlineCode(a.Identity())},
		{"Environment", a.Environment},
		{"Target", a.Target},
		{"Observed", formatAlertTime(a.ObservedAt)},
	}
	for _, row := range rows {
		if strings.TrimSpace(row[1]) != "" {
			fmt.Fprintf(&b, "- **%s:** %s\n", row[0], markdownLine(row[1]))
		}
	}
	b.WriteString("\n")

	writeMarkdownSection(&b, "Symptom", a.Symptom)
	writeMarkdownSection(&b, "Mechanism", a.Mechanism)
	writeMarkdownSection(&b, "Expected baseline", a.Baseline)
	writeMarkdownSection(&b, "Observed values", a.Observed)
	writeMarkdownSection(&b, "Evidence", a.Evidence)
	writeMarkdownSection(&b, "Context", a.Context)
	writeMarkdownSection(&b, "Action", a.Action)
	writeMarkdownSection(&b, "Verify", a.Verify)
	writeMarkdownSection(&b, "Playbook", a.Playbook)
	return strings.TrimSpace(b.String()) + "\n"
}

// ToMarkdown is an explicit conversion alias for callers that prefer a
// verb-shaped API.
func (a Alert) ToMarkdown() string { return a.Markdown() }

// String makes an Alert human-readable in logs without discarding structure.
func (a Alert) String() string { return a.Markdown() }

// AlertsMarkdown renders a deterministic Markdown alert file. Alerts are
// sorted by severity and stable identity so repeated runs produce reviewable
// diffs even when probes complete in a different order.
func AlertsMarkdown(alerts []Alert) string {
	ordered := append([]Alert(nil), alerts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Severity != ordered[j].Severity {
			return ordered[i].Severity == SeverityPage
		}
		return ordered[i].Identity() < ordered[j].Identity()
	})

	var b strings.Builder
	b.WriteString("# Monitor alerts\n\n")
	if len(ordered) == 0 {
		b.WriteString("No active alerts.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%d active alert(s).\n\n", len(ordered))
	for i, alert := range ordered {
		if i != 0 {
			b.WriteString("\n---\n\n")
		}
		b.WriteString(alert.Markdown())
	}
	return b.String()
}

// WriteAlertsMarkdown writes the same document returned by AlertsMarkdown.
func WriteAlertsMarkdown(w io.Writer, alerts []Alert) error {
	_, err := io.WriteString(w, AlertsMarkdown(alerts))
	return err
}

func signalReference(a Alert) string {
	parts := []string{}
	if a.SignalNumber != "" {
		reference := "SIGNALS.md §" + a.SignalNumber
		if a.SignalKey != "" {
			reference += " (`" + a.SignalKey + "`)"
		}
		parts = append(parts, reference)
	}
	if a.SignalID != "" {
		parts = append(parts, inlineCode(a.SignalID))
	}
	if a.SignalName != "" {
		parts = append(parts, a.SignalName)
	}
	return strings.Join(parts, " — ")
}

func writeMarkdownSection(b *strings.Builder, heading, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	fmt.Fprintf(b, "### %s\n\n%s\n\n", heading, body)
}

func formatAlertTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func inlineCode(s string) string {
	if s == "" {
		return ""
	}
	return "`" + strings.ReplaceAll(s, "`", "\\`") + "`"
}

func markdownLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
