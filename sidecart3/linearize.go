package sidecart3

// §22.8's history rule (TEST-M2-02; D-TEST-10, FI-2): "Porcupine checks T3 histories per ref with §11.8's commit
// rule; an unknown outcome has an open end time and matches any result". CheckLinearizable takes the plain history
// sidecartest/fault records (fault.History), partitions it per ref, gives every unknown outcome an open end, and asks
// Porcupine whether some order of each ref's operations, each placed between its call and its return, obeys the rule.
// Porcupine is imported here, where only server's tests reach it, and never by the sidecar module
// (plan-TEST-small-readings reading 2, QD-303; C-18).
//
// The rule is EVS1-M2-42's model of §11.8's commit rule (server/evshost/porcupine_test.go), carried here so that a
// check outside that package runs it, and extended to head reads. A ref's state is its version, the push id each
// version committed and the push ids refused, from version 0, so a history holds its refs from their first push:
//
//	commit    a push answered committed at v: v is the next version and its push id has neither committed nor been
//	          refused, which commits it there; or its push id committed at v already, a retry answering its original
//	          version (§11.8's 5a). A second version for one push id, a version another push holds, and a skipped
//	          version are no step.
//	abort     a push answered refused: its push id has not committed, and from here never does (I-EVS-3). A SHREDDING
//	          refusal binds nothing: FI-2 lets an attempt validated before its repo's shred began commit after another's
//	          SHREDDING refusal, since the shred destroys it, and which attempt was validated when is FI-2's oracle's to
//	          judge (TEST-M2-04), not a history's.
//	unknown   a push of unknown outcome matches either: it commits at the next version, or does nothing. Once its push
//	          id has committed or been refused it does nothing.
//	read      a head read served v: the ref is at v, and v's push is the one the read names, when it names one. A read
//	          of unknown outcome matches anything.
//
// A history that holds no unknown outcome is refused as no evidence: a run whose every answer arrived proves little
// of a rule whose hard part is the answer that did not, and §22.10's M2 gate asks for histories with unknown
// outcomes, as EVS1-M2-42 also says. A malformed operation fails the check rather than being left out, and so does a
// ref Porcupine cannot settle within its time, since a search that stopped found nothing either way.
//
// A ref that does not linearize is rendered for a person into the settings' directory: a text file with the failing
// prefix, the longest order of the ref's operations the rule accepts, the state it leaves and the operations no order
// places after it, then every operation of the ref by call; and Porcupine's visualization of the same search, as HTML.

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/anishathalye/porcupine"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest/fault"
)

type LinearizeSettings struct {
	// How long Porcupine may search one ref's operations. A ref it has not settled by then fails the check, since a
	// search that stopped found nothing either way. Non-positive searches for as long as it takes.
	RefTimeout time.Duration
	// Where a ref that does not linearize is rendered. Empty renders into a new directory under the system's
	// temporary directory, which the error names; a test passes its t.ArtifactDir().
	RenderDirectory string
}

// A minute a ref, EVS1-M2-42's bound for a whole row's history, which FS-1's take well inside.
func DefaultLinearizeSettings() *LinearizeSettings {
	return &LinearizeSettings{
		RefTimeout: time.Minute,
	}
}

var (
	// The history holds no unknown outcome.
	ErrNoUnknownOutcome = errors.New("no unknown outcome, so the history proves little and is refused as no evidence")
	// No order of a ref's operations obeys §11.8's commit rule.
	ErrNotLinearizable = errors.New("not linearizable under §11.8's commit rule")
	// Porcupine had not settled a ref within LinearizeSettings.RefTimeout.
	ErrLinearizeUndecided = errors.New("not settled within the check's time, which is no pass")
)

// What a check found for one ref.
type LinearizeRef struct {
	Ref            fault.HistoryRef
	OperationCount int
	UnknownCount   int
	Result         porcupine.CheckResult
	// The files it was rendered to, when it did not linearize: the text first, then the visualization.
	RenderingPaths []string
}

// What a check found.
type LinearizeReport struct {
	Name string
	Refs []*LinearizeRef
	// The history's end, which every unknown outcome's open end was set to.
	End int64
}

// Over every ref.
func (self *LinearizeReport) OperationCount() int {
	count := 0
	for _, ref := range self.Refs {
		count += ref.OperationCount
	}
	return count
}

// Over every ref.
func (self *LinearizeReport) UnknownCount() int {
	count := 0
	for _, ref := range self.Refs {
		count += ref.UnknownCount
	}
	return count
}

// For a test's log: each ref's operations, unknowns and result.
func (self *LinearizeReport) String() string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s: %d operations on %d refs, %d unknown", self.Name, self.OperationCount(), len(self.Refs), self.UnknownCount())
	for _, ref := range self.Refs {
		fmt.Fprintf(&text, "\n  %s: %d operations, %d unknown, %s", ref.Ref, ref.OperationCount, ref.UnknownCount, ref.Result)
		for _, renderingPath := range ref.RenderingPaths {
			fmt.Fprintf(&text, "\n    %s", renderingPath)
		}
	}
	return text.String()
}

// Whether history linearizes under §11.8's commit rule, ref by ref (the file header). The error is nil only when the
// history is well formed, holds an unknown outcome and every ref linearizes; it wraps ErrNoUnknownOutcome,
// ErrNotLinearizable or ErrLinearizeUndecided for each reason that applies, naming each failing ref and its renderings.
// settings nil takes the defaults.
func CheckLinearizable(history *fault.History, settings *LinearizeSettings) (*LinearizeReport, error) {
	if settings == nil {
		settings = DefaultLinearizeSettings()
	}
	report := &LinearizeReport{Name: history.Name()}
	if err := history.Validate(); err != nil {
		return report, fmt.Errorf("%s holds operations no check can read: %w", history.Name(), err)
	}
	operations := history.Operations()
	for _, operation := range operations {
		report.End = max(report.End, operation.Call, operation.Return)
	}
	// an open end: after every call and return the history holds
	report.End += 1

	errs := []error{}
	if !slices.ContainsFunc(operations, func(operation *fault.Operation) bool { return operation.Outcome == fault.OutcomeUnknown }) {
		errs = append(errs, fmt.Errorf("%s: %w", history.Name(), ErrNoUnknownOutcome))
	}
	renderDirectory := settings.RenderDirectory
	for _, ref := range history.Refs() {
		refOperations := []*fault.Operation{}
		for _, operation := range operations {
			if operation.Ref == ref {
				refOperations = append(refOperations, operation)
			}
		}
		checked := &LinearizeRef{Ref: ref, OperationCount: len(refOperations)}
		report.Refs = append(report.Refs, checked)
		for _, operation := range refOperations {
			if operation.Outcome == fault.OutcomeUnknown {
				checked.UnknownCount += 1
			}
		}
		porcupineOperations := linearizeOperations(refOperations, report.End)
		model := commitRule()
		checked.Result = porcupine.CheckOperationsTimeout(model, porcupineOperations, settings.RefTimeout)
		if checked.Result == porcupine.Ok {
			continue
		}
		reason := ErrNotLinearizable
		if checked.Result == porcupine.Unknown {
			reason = ErrLinearizeUndecided
		}
		if renderDirectory == "" {
			directory, err := os.MkdirTemp("", "sidecart3-linearize-")
			if err != nil {
				errs = append(errs, fmt.Errorf("%s at %s: %w, and no directory to render it in: %v", history.Name(), ref, reason, err))
				continue
			}
			renderDirectory = directory
		}
		renderingPaths, err := renderLinearization(renderDirectory, history.Name(), ref, model, porcupineOperations, settings.RefTimeout)
		checked.RenderingPaths = renderingPaths
		if err != nil {
			errs = append(errs, fmt.Errorf("%s at %s: %w, and its rendering failed: %v", history.Name(), ref, reason, err))
			continue
		}
		errs = append(errs, fmt.Errorf("%s at %s: %w, rendered in %s", history.Name(), ref, reason, strings.Join(renderingPaths, " and ")))
	}
	return report, errors.Join(errs...)
}

// What the model is called with: a push's id, or a read.
type linearizeInput struct {
	kind   fault.OperationKind
	pushId sidecar.Id
}

// What the model is answered: the outcome, a commit's or a read's version, the push a read's version names, and a
// refusal's code.
type linearizeOutput struct {
	outcome fault.Outcome
	version uint64
	pushId  sidecar.Id
	code    protocol.EvsRefusalCode
}

// One ref's operations as Porcupine's, each unknown outcome's end open at end. Each operation runs on the lowest
// client lane free at its call, so a lane's operations never overlap and the visualization draws one per row.
func linearizeOperations(operations []*fault.Operation, end int64) []porcupine.Operation {
	ordered := slices.Clone(operations)
	slices.SortStableFunc(ordered, func(a *fault.Operation, b *fault.Operation) int {
		return cmp.Compare(a.Call, b.Call)
	})
	laneEnds := []int64{}
	porcupineOperations := make([]porcupine.Operation, 0, len(ordered))
	for _, operation := range ordered {
		returned := operation.Return
		if operation.Outcome == fault.OutcomeUnknown {
			returned = end
		}
		lane := slices.IndexFunc(laneEnds, func(laneEnd int64) bool { return laneEnd < operation.Call })
		if lane < 0 {
			lane = len(laneEnds)
			laneEnds = append(laneEnds, 0)
		}
		laneEnds[lane] = returned
		input := linearizeInput{kind: operation.Kind}
		output := linearizeOutput{outcome: operation.Outcome, version: operation.Version, code: operation.Code}
		if operation.Kind == fault.OperationPush {
			input.pushId = operation.PushId
		} else {
			output.pushId = operation.PushId
		}
		porcupineOperations = append(porcupineOperations, porcupine.Operation{
			ClientId: lane,
			Input:    input,
			Call:     operation.Call,
			Output:   output,
			Return:   returned,
			Metadata: operation,
		})
	}
	return porcupineOperations
}

// One ref's state under the rule. Never changed once made: a step makes a new one.
type commitRuleState struct {
	version uint64
	// SHA-256 over the push ids committed so far, in version order, so equal chains committed the same pushes at the
	// same versions
	chain [32]byte
	// the push id each version committed, version v at v-1
	versionPushIds []sidecar.Id
	pushIdVersions map[sidecar.Id]uint64
	refused        map[sidecar.Id]bool
	// over the refused push ids, in no order
	refusedHash uint64
}

// A ref with no push yet, at version 0.
func newCommitRuleState() *commitRuleState {
	return &commitRuleState{pushIdVersions: map[sidecar.Id]uint64{}, refused: map[sidecar.Id]bool{}}
}

// The state after pushId commits at the next version.
func (self *commitRuleState) commit(pushId sidecar.Id) *commitRuleState {
	next := &commitRuleState{
		version:        self.version + 1,
		chain:          sha256.Sum256(append(self.chain[:], pushId[:]...)),
		versionPushIds: append(slices.Clone(self.versionPushIds), pushId),
		pushIdVersions: make(map[sidecar.Id]uint64, len(self.pushIdVersions)+1),
		refused:        self.refused,
		refusedHash:    self.refusedHash,
	}
	for id, version := range self.pushIdVersions {
		next.pushIdVersions[id] = version
	}
	next.pushIdVersions[pushId] = next.version
	return next
}

// The state after pushId's refusal.
func (self *commitRuleState) refuse(pushId sidecar.Id) *commitRuleState {
	if self.refused[pushId] {
		return self
	}
	next := &commitRuleState{
		version:        self.version,
		chain:          self.chain,
		versionPushIds: self.versionPushIds,
		pushIdVersions: self.pushIdVersions,
		refused:        make(map[sidecar.Id]bool, len(self.refused)+1),
		refusedHash:    self.refusedHash ^ commitRuleIdHash(pushId),
	}
	for id := range self.refused {
		next.refused[id] = true
	}
	next.refused[pushId] = true
	return next
}

// An id's part of a state's hash.
func commitRuleIdHash(id sidecar.Id) uint64 {
	hash := fnv.New64a()
	hash.Write(id[:])
	return hash.Sum64()
}

// Whether two states committed the same pushes at the same versions and refused the same ones.
func (self *commitRuleState) equal(other *commitRuleState) bool {
	if self.version != other.version || self.chain != other.chain || self.refusedHash != other.refusedHash || len(self.refused) != len(other.refused) {
		return false
	}
	for id := range self.refused {
		if !other.refused[id] {
			return false
		}
	}
	return true
}

// Equal for equal states, as Porcupine's cache needs.
func (self *commitRuleState) hash() uint64 {
	return self.version ^ binary.BigEndian.Uint64(self.chain[:8]) ^ self.refusedHash
}

// "v3, 1 refused": the version, and how many push ids are refused.
func (self *commitRuleState) String() string {
	if len(self.refused) == 0 {
		return fmt.Sprintf("v%d", self.version)
	}
	return fmt.Sprintf("v%d, %d refused", self.version, len(self.refused))
}

// The states state may step to on input answered output: none when the rule forbids it.
func commitRuleStep(state *commitRuleState, input linearizeInput, output linearizeOutput) []*commitRuleState {
	if input.kind == fault.OperationRead {
		if output.outcome == fault.OutcomeUnknown {
			return []*commitRuleState{state}
		}
		if output.outcome != fault.OutcomeCommitted || output.version != state.version {
			return nil
		}
		if output.pushId != (sidecar.Id{}) && (output.version == 0 || state.versionPushIds[output.version-1] != output.pushId) {
			return nil
		}
		return []*commitRuleState{state}
	}
	committedAt, committed := state.pushIdVersions[input.pushId]
	refused := state.refused[input.pushId]
	switch output.outcome {
	case fault.OutcomeCommitted:
		switch {
		case committed && committedAt == output.version:
			return []*commitRuleState{state}
		case !committed && !refused && output.version == state.version+1:
			return []*commitRuleState{state.commit(input.pushId)}
		}
		return nil
	case fault.OutcomeAborted:
		if output.code == protocol.EvsRefusalCode_EVS_REFUSAL_SHREDDING {
			return []*commitRuleState{state}
		}
		if committed {
			return nil
		}
		return []*commitRuleState{state.refuse(input.pushId)}
	case fault.OutcomeUnknown:
		if committed || refused {
			return []*commitRuleState{state}
		}
		return []*commitRuleState{state, state.commit(input.pushId)}
	}
	return nil
}

// §11.8's commit rule as a nondeterministic sequential model of one ref (the file header).
func commitRule() porcupine.Model {
	model := &porcupine.NondeterministicModel{
		Init: func() []any {
			return []any{newCommitRuleState()}
		},
		Step: func(state any, input any, output any) []any {
			next := []any{}
			for _, each := range commitRuleStep(state.(*commitRuleState), input.(linearizeInput), output.(linearizeOutput)) {
				next = append(next, each)
			}
			return next
		},
		Equal: func(a any, b any) bool {
			return a.(*commitRuleState).equal(b.(*commitRuleState))
		},
		Hash: func(state any) uint64 {
			return state.(*commitRuleState).hash()
		},
		DescribeOperation: func(input any, output any) string {
			return linearizeDescribe(input.(linearizeInput), output.(linearizeOutput))
		},
		DescribeState: func(state any) string {
			return state.(*commitRuleState).String()
		},
		DescribeOperationMetadata: func(metadata any) string {
			if operation, ok := metadata.(*fault.Operation); ok {
				return operation.String()
			}
			return ""
		},
	}
	return model.ToModel()
}

// "push <id> -> committed v3", "read -> v3 by <id>".
func linearizeDescribe(input linearizeInput, output linearizeOutput) string {
	if input.kind == fault.OperationRead {
		switch {
		case output.outcome != fault.OutcomeCommitted:
			return fmt.Sprintf("read -> %s", output.outcome)
		case output.pushId != (sidecar.Id{}):
			return fmt.Sprintf("read -> v%d by %s", output.version, output.pushId)
		}
		return fmt.Sprintf("read -> v%d", output.version)
	}
	if output.outcome == fault.OutcomeCommitted {
		return fmt.Sprintf("push %s -> committed v%d", input.pushId, output.version)
	}
	return fmt.Sprintf("push %s -> %s", input.pushId, output.outcome)
}

// Renders a ref that did not linearize into directory, as the text of its failing prefix and Porcupine's
// visualization, and returns their paths: the file header.
func renderLinearization(directory string, name string, ref fault.HistoryRef, model porcupine.Model, operations []porcupine.Operation, timeout time.Duration) ([]string, error) {
	result, info := porcupine.CheckOperationsVerbose(model, operations, timeout)
	base := linearizeFileName(name, ref)
	textFile, err := os.CreateTemp(directory, base+"-*.txt")
	if err != nil {
		return nil, err
	}
	textPath := textFile.Name()
	renderingPaths := []string{textPath}
	_, writeErr := textFile.WriteString(linearizeText(name, ref, model, operations, result, info))
	closeErr := textFile.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return renderingPaths, err
	}
	htmlPath := strings.TrimSuffix(textPath, ".txt") + ".html"
	if err := porcupine.VisualizePath(model, info, htmlPath); err != nil {
		return renderingPaths, err
	}
	return append(renderingPaths, htmlPath), nil
}

// A file name from a history's name and a ref: the name's letters, digits, dots and dashes, the rest dashes, then
// the first 8 hex digits of the repo's id and of the ref's.
func linearizeFileName(name string, ref fault.HistoryRef) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9', r == '.', r == '-':
			return r
		}
		return '-'
	}, name)
	return fmt.Sprintf("%s-%s-%s", safe, sidecar.EvsKeyId(ref.RepoId)[:8], sidecar.EvsKeyId(ref.RefId)[:8])
}

// The failing prefix as text: the longest order the rule accepts, the state it leaves, what no order places next, and
// every operation by call.
func linearizeText(name string, ref fault.HistoryRef, model porcupine.Model, operations []porcupine.Operation, result porcupine.CheckResult, info porcupine.LinearizationInfo) string {
	var text strings.Builder
	unknownCount := 0
	for _, operation := range operations {
		if operation.Output.(linearizeOutput).outcome == fault.OutcomeUnknown {
			unknownCount += 1
		}
	}
	fmt.Fprintf(&text, "%s at %s: %s under §11.8's commit rule\n", name, ref, result)
	fmt.Fprintf(&text, "%d operations, %d unknown, each unknown outcome's end open\n", len(operations), unknownCount)

	prefix := []porcupine.Operation{}
	partitions := info.PartialLinearizationsOperations()
	if 0 < len(partitions) {
		for _, partial := range partitions[0] {
			if len(prefix) < len(partial) {
				prefix = partial
			}
		}
	}
	fmt.Fprintf(&text, "\nthe failing prefix: the longest order of the ref's operations the rule accepts, %d of %d\n", len(prefix), len(operations))
	state := model.Init()
	for index, operation := range prefix {
		_, state = model.Step(state, operation.Input, operation.Output)
		fmt.Fprintf(&text, "  %4d  %s\n", index+1, linearizeOperationText(operation))
	}
	fmt.Fprintf(&text, "the state it leaves: %s\n", model.DescribeState(state))

	placedOperations := map[*fault.Operation]bool{}
	for _, operation := range prefix {
		placedOperations[operation.Metadata.(*fault.Operation)] = true
	}
	fmt.Fprintf(&text, "\nwhat no order places after it, by call\n")
	for _, operation := range operations {
		if !placedOperations[operation.Metadata.(*fault.Operation)] {
			fmt.Fprintf(&text, "        %s\n", linearizeOperationText(operation))
		}
	}
	fmt.Fprintf(&text, "\nevery operation of the ref, by call\n")
	for _, operation := range operations {
		fmt.Fprintf(&text, "        %s\n", linearizeOperationText(operation))
	}
	return text.String()
}

// The history's own text for a Porcupine operation made from it.
func linearizeOperationText(operation porcupine.Operation) string {
	return operation.Metadata.(*fault.Operation).String()
}
