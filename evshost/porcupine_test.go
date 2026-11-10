//go:build sidecar_localstack && sidecar_testhooks

// §22.8's history row (EVS1-M2-42): Porcupine checks a ref's push history against §11.8's commit rule. Each attempt
// an evs instance took of a push is one operation, from when the instance began it to when it answered, read from
// the evs world's logical clock (fault.EvsWorld.Attempts): a 200 names the version its push id committed at, a
// refusal says that push id never commits, an answer that refuses nothing changed nothing, and an unknown outcome,
// a 503 {outcome: unknown} or an attempt its instance never answered, has an open end and matches either. The
// sequential rule each history must linearize to: versions rise by one; a push id commits at most once, and an
// attempt of a committed one answers its version; a refused push id never commits, and no attempt of a committed one
// is refused (I-EVS-3, FI-2).
//
// The histories are FS-1's and FS-58's schedules, on T3's Nets through this package's block and on T2's fakes, each
// run again with one write's answer lost, as §22.8's unknown rule loses it, since a pass on a run with no unknown
// outcome proves little: each history checked must hold one. A history built by hand, a refused push that commits,
// shows the model refuses what the rule forbids.

package evshost_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anishathalye/porcupine"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

// What an attempt's answer says of its push.
type porcupineOutcome int

const (
	porcupineCommitted porcupineOutcome = iota + 1
	porcupineRefused
	// refuses nothing and changed nothing: a 429, a 400, a 503 that is not unknown
	porcupineNothing
	porcupineUnknown
)

type porcupineOutput struct {
	outcome porcupineOutcome
	version uint64
}

// One ref's state under the rule: its version, the version each push id committed at, the refused push ids.
type porcupineState struct {
	version   uint64
	committed map[sidecar.Id]uint64
	refused   map[sidecar.Id]bool
}

func (self *porcupineState) commit(pushId sidecar.Id) *porcupineState {
	next := &porcupineState{version: self.version + 1, committed: map[sidecar.Id]uint64{}, refused: self.refused}
	for id, version := range self.committed {
		next.committed[id] = version
	}
	next.committed[pushId] = next.version
	return next
}

func (self *porcupineState) refuse(pushId sidecar.Id) *porcupineState {
	next := &porcupineState{version: self.version, committed: self.committed, refused: map[sidecar.Id]bool{}}
	for id := range self.refused {
		next.refused[id] = true
	}
	next.refused[pushId] = true
	return next
}

// A state's text, which Equal and Hash compare.
func (self *porcupineState) String() string {
	parts := []string{fmt.Sprintf("v%d", self.version)}
	for id, version := range self.committed {
		parts = append(parts, fmt.Sprintf("c%s@%d", id, version))
	}
	for id := range self.refused {
		parts = append(parts, "r"+id.String())
	}
	slices.Sort(parts[1:])
	return strings.Join(parts, " ")
}

// §11.8's commit rule as a nondeterministic sequential model: an unknown outcome steps both ways.
func porcupineCommitRule() porcupine.Model {
	model := &porcupine.NondeterministicModel{
		Init: func() []interface{} {
			return []interface{}{&porcupineState{committed: map[sidecar.Id]uint64{}, refused: map[sidecar.Id]bool{}}}
		},
		Step: func(state interface{}, input interface{}, output interface{}) []interface{} {
			current := state.(*porcupineState)
			pushId := input.(sidecar.Id)
			out := output.(porcupineOutput)
			committedAt, committed := current.committed[pushId]
			refused := current.refused[pushId]
			switch out.outcome {
			case porcupineCommitted:
				switch {
				case committed && committedAt == out.version:
					return []interface{}{current}
				case !committed && !refused && out.version == current.version+1:
					return []interface{}{current.commit(pushId)}
				}
				return nil
			case porcupineRefused:
				if committed {
					return nil
				}
				return []interface{}{current.refuse(pushId)}
			case porcupineNothing:
				return []interface{}{current}
			}
			if committed || refused {
				return []interface{}{current}
			}
			return []interface{}{current, current.commit(pushId)}
		},
		Equal: func(a interface{}, b interface{}) bool {
			return a.(*porcupineState).String() == b.(*porcupineState).String()
		},
		DescribeOperation: func(input interface{}, output interface{}) string {
			return fmt.Sprintf("%s -> %v", input.(sidecar.Id), output)
		},
	}
	return model.ToModel()
}

// A world's attempts as Porcupine's operations, the unanswered with an open end, and how many outcomes were unknown.
func porcupineHistory(attempts []*fault.EvsWorldAttempt, end int64) ([]porcupine.Operation, int) {
	operations := []porcupine.Operation{}
	unknownCount := 0
	for index, attempt := range attempts {
		operation := porcupine.Operation{ClientId: index, Input: attempt.PushId, Call: attempt.Call, Return: attempt.Return}
		answer := attempt.Answer
		switch {
		case answer == nil:
			operation.Output = porcupineOutput{outcome: porcupineUnknown}
			operation.Return = end
		case answer.Status == http.StatusOK:
			operation.Output = porcupineOutput{outcome: porcupineCommitted, version: answer.Version}
		case answer.Unknown:
			operation.Output = porcupineOutput{outcome: porcupineUnknown}
			operation.Return = end
		case answer.Refuses():
			operation.Output = porcupineOutput{outcome: porcupineRefused}
		default:
			operation.Output = porcupineOutput{outcome: porcupineNothing}
		}
		if operation.Output.(porcupineOutput).outcome == porcupineUnknown {
			unknownCount += 1
		}
		operations = append(operations, operation)
	}
	return operations, unknownCount
}

// One run of builder on a Net from settings, the lostNth write of op with its answer lost when lostNth is above zero,
// its world's history checked: the run passes, holds an unknown outcome when requireUnknown says it must, and
// linearizes.
func porcupineCheckRun(t testing.TB, builder fault.Builder, settings *sidecartest.NetSettings, op string, lostNth int, requireUnknown bool) {
	t.Helper()
	net := sidecartest.NewNet(t.Context(), settings)
	defer net.Close()
	schedule := builder(net)
	schedule.Twin = nil
	schedule.Refuses = ""
	if 0 < lostNth {
		plan := &fault.FaultPlan{Op: op, Nth: lostNth, Result: fault.ApplyThenDrop, Displaces: true, WritesOnly: true}
		schedule.Steps = append([]*fault.Step{{Fault: plan}}, schedule.Steps...)
		schedule.Id = fmt.Sprintf("%s with %s write %d's answer lost", schedule.Id, op, lostNth)
	}
	result := fault.Run(t.Context(), schedule, net)
	if result.Err != nil {
		t.Fatalf("%s", result)
	}
	world := fault.EvsWorldOf(net)
	if world == nil {
		t.Fatalf("%s made no evs world", schedule.Id)
	}
	history, unknownCount := porcupineHistory(world.Attempts(), world.AttemptClock())
	if requireUnknown && unknownCount == 0 {
		t.Fatalf("%s: a history with no unknown outcome, which proves little", schedule.Id)
	}
	if outcome := porcupine.CheckOperationsTimeout(porcupineCommitRule(), history, time.Minute); outcome != porcupine.Ok {
		t.Fatalf("%s: %d attempts are %s under §11.8's commit rule", schedule.Id, len(history), outcome)
	}
	t.Logf("%s: %d attempts linearize, %d unknown", schedule.Id, len(history), unknownCount)
}

// FS-1's and FS-58's runs on tier's Nets: FS-1 as written, whose head CAS loses its answer to a killed instance, and
// again with each of a spread of its writes lost, every one holding that unknown outcome; FS-58's, whose 412 reruns
// and 409 race a fold, with a spread of its writes lost, which its instances resolve, so its histories may hold none.
func porcupineCheckRows(t testing.TB, settings *sidecartest.NetSettings, spread int) {
	t.Helper()
	porcupineCheckRun(t, fault.Fs1LostAnswerRetriedOnEvs2, settings, "", 0, true)
	for nth := 1; nth <= 200; nth += spread {
		porcupineCheckRun(t, fault.Fs1LostAnswerRetriedOnEvs2, settings, sidecartest.FaultOpPut, nth, true)
	}
	for _, builder := range []fault.Builder{fault.Fs58FoldBetweenReadAndRerun, fault.Fs58DisjointRerun} {
		porcupineCheckRun(t, builder, settings, "", 0, false)
		for nth := 1; nth <= 12; nth += 3 {
			porcupineCheckRun(t, builder, settings, sidecartest.FaultOpPut, nth, false)
		}
	}
}

func TestPorcupineHistoriesOnT3(t *testing.T) {
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		porcupineCheckRows(tb, sidecart3.NetSettings(tb, nil), 50)
	})
}

func TestPorcupineHistoriesOnT2(t *testing.T) {
	porcupineCheckRows(t, nil, 10)
}

// The model refuses what the rule forbids: a push answered refused by one instance and committed by another.
func TestPorcupineRefusesARefusedPushThatCommits(t *testing.T) {
	pushId := sidecar.NewId()
	history := []porcupine.Operation{
		{ClientId: 0, Input: pushId, Call: 1, Output: porcupineOutput{outcome: porcupineRefused}, Return: 2},
		{ClientId: 1, Input: pushId, Call: 3, Output: porcupineOutput{outcome: porcupineCommitted, version: 1}, Return: 4},
	}
	if porcupine.CheckOperations(porcupineCommitRule(), history) {
		t.Fatal("a refused push's later commit linearized")
	}
	// the same with the commit first: its version answered again is a retry, a refusal after it is not
	retried := []porcupine.Operation{
		{ClientId: 0, Input: pushId, Call: 1, Output: porcupineOutput{outcome: porcupineCommitted, version: 1}, Return: 2},
		{ClientId: 1, Input: pushId, Call: 3, Output: porcupineOutput{outcome: porcupineCommitted, version: 1}, Return: 4},
	}
	if !porcupine.CheckOperations(porcupineCommitRule(), retried) {
		t.Fatal("a retry answering the original version did not linearize")
	}
	if porcupine.CheckOperations(porcupineCommitRule(), append(retried, porcupine.Operation{ClientId: 2, Input: pushId, Call: 5, Output: porcupineOutput{outcome: porcupineRefused}, Return: 6})) {
		t.Fatal("a committed push's refusal linearized")
	}
	// an unknown outcome open to the end matches a commit another attempt answers
	other := sidecar.NewId()
	unknown := []porcupine.Operation{
		{ClientId: 0, Input: other, Call: 1, Output: porcupineOutput{outcome: porcupineUnknown}, Return: 10},
		{ClientId: 1, Input: other, Call: 2, Output: porcupineOutput{outcome: porcupineCommitted, version: 1}, Return: 3},
	}
	if !porcupine.CheckOperations(porcupineCommitRule(), unknown) {
		t.Fatal("an unknown attempt beside its push's commit did not linearize")
	}
	// two push ids at one version cannot both commit
	twice := []porcupine.Operation{
		{ClientId: 0, Input: pushId, Call: 1, Output: porcupineOutput{outcome: porcupineCommitted, version: 1}, Return: 2},
		{ClientId: 1, Input: other, Call: 3, Output: porcupineOutput{outcome: porcupineCommitted, version: 1}, Return: 4},
	}
	if porcupine.CheckOperations(porcupineCommitRule(), twice) {
		t.Fatal("two pushes committed at one version")
	}
}
