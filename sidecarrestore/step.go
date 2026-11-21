package sidecarrestore

// The frame's order and its runner (§19.10). A site incident qualifies replacement storage before ops reads it.
// Its authority step resumes configuration logs before daemons can re-register during rows. A step registers
// its own function. The operator records the completion of manual steps with EndStepByHand.
//
// A step runs only once every step before it in the order has ended under its incident, and only in an incident of
// its kind: the site steps refuse a Postgres incident. Every step is idempotent and keyed by the incident id, so a
// step that ended may run again, and a run that fails midway is run again from the start.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"
)

// The step names recorded in an incident.
const (
	StepOps       = "ops"
	StepAbuse     = "abuse"
	StepDns       = "dns"
	StepApi       = "api"
	StepLabels    = "labels"
	StepRows      = "rows"
	StepBilling   = "billing"
	StepHardware  = "hardware"
	StepAuthority = "authority"
	StepGroups    = "groups"
	StepRepos     = "repos"
	StepDrops     = "drops"
	StepClose     = "close"
)

// One place in the order.
type frameStepPlace struct {
	name string
	// runs only after a site loss
	siteOnly bool
}

// The incident list and both dumps already exist. Ops and api need an answering metadata cluster.
func frameOrder() []frameStepPlace {
	return []frameStepPlace{
		{name: StepHardware, siteOnly: true},
		{name: StepOps},
		{name: StepAbuse},
		{name: StepDns},
		{name: StepApi},
		{name: StepLabels},
		{name: StepAuthority, siteOnly: true},
		{name: StepRows},
		{name: StepBilling},
		{name: StepGroups, siteOnly: true},
		{name: StepRepos, siteOnly: true},
		{name: StepDrops, siteOnly: true},
		{name: StepClose, siteOnly: true},
	}
}

// What a step's function gets for one run.
type StepRun struct {
	Incident *Incident
	// progress lines for the operator, ids and counts only
	Out io.Writer
	// what the operator stated by hand for this run, such as how restore_hold is held where the step needs it; empty
	// for none
	Statement string
	// the named inputs the operator gave this run, such as the abuse step's mailbox exports; each step names the
	// ones it reads, and nil is none
	Inputs map[string]string
	// whether the operator asked the step to act: a step that prints its plan first, as the abuse step does,
	// changes nothing and does not end until a run says so
	Apply bool
}

// What the operator gives a run beside the incident.
type StepOptions struct {
	// see StepRun.Statement
	Statement string
	// see StepRun.Inputs
	Inputs map[string]string
	// see StepRun.Apply
	Apply bool
}

// The input name names, or ErrStepInput naming it when the operator gave none.
func (self *StepRun) RequireInput(name string) (string, error) {
	value := self.Inputs[name]
	if value == "" {
		return "", fmt.Errorf("%w: give --input %s=<value>", ErrStepInput, name)
	}
	return value, nil
}

// One run of a step under its incident. It must be idempotent: it may run again after it ended, and again after it
// failed partway.
type StepFunction func(ctx context.Context, run *StepRun) error

// A step as the frame holds it.
type Step struct {
	Name string
	// runs only after a site loss
	SiteOnly bool
	// nil while no task has registered it
	Function StepFunction
}

var (
	// No step has the name.
	ErrStepUnknown = errors.New("no restore step has that name")
	// A step that runs only after a site loss, in a Postgres incident.
	ErrStepSiteOnly = errors.New("the step runs only after a site loss")
	// A step before it in the order has not ended under the incident.
	ErrStepOutOfOrder = errors.New("a step before it has not ended under the incident")
	// No task has registered the step's function yet: it is run by hand and its end stated by hand.
	ErrStepUnregistered = errors.New("no function is registered for the step")
	// The incident records no kind yet, so the frame cannot tell which steps it runs.
	ErrIncidentKindUnknown = errors.New("the incident records no kind")
	// The step needs an input the operator did not give.
	ErrStepInput = errors.New("the step needs an input")
	// The step printed its plan and changed nothing, since the run did not ask it to act.
	ErrStepPlanOnly = errors.New("the step printed its plan and changed nothing")
)

// The order and each step's function. Safe for concurrent use.
type Frame struct {
	stateLock sync.Mutex
	functions map[string]StepFunction
	// the clock a step's end is recorded at
	now func() time.Time
}

// A frame with no function registered.
func NewFrame() *Frame {
	return &Frame{
		functions: map[string]StepFunction{},
		now:       time.Now,
	}
}

// The frame this process's steps register with, which sidecarctl's sidecar restore runs.
var defaultFrame = NewFrame()

// The process's frame.
func DefaultFrame() *Frame {
	return defaultFrame
}

// Registers a step's function with the process's frame. Call it from the init of the step's own file. An unknown
// name, a nil function and a second function for one step panic, since each would leave a step unreachable.
func RegisterStep(name string, function StepFunction) {
	defaultFrame.Register(name, function)
}

// Registers name's function. See RegisterStep.
func (self *Frame) Register(name string, function StepFunction) {
	if !slices.ContainsFunc(frameOrder(), func(place frameStepPlace) bool { return place.name == name }) {
		panic(fmt.Sprintf("restore step %q is not one of §19.10's", name))
	}
	if function == nil {
		panic(fmt.Sprintf("restore step %q with no function", name))
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.functions[name]; ok {
		panic(fmt.Sprintf("a second function for restore step %q", name))
	}
	self.functions[name] = function
}

// Every step in order, with its function where one is registered.
func (self *Frame) Steps() []*Step {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	steps := []*Step{}
	for _, place := range frameOrder() {
		steps = append(steps, &Step{Name: place.name, SiteOnly: place.siteOnly, Function: self.functions[place.name]})
	}
	return steps
}

// The step named name, ErrStepUnknown for none.
func (self *Frame) step(name string) (*Step, error) {
	for _, step := range self.Steps() {
		if step.Name == name {
			return step, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrStepUnknown, name)
}

// The steps for this incident, including replacement storage and re-seeding only for a site loss.
func (self *Frame) incidentSteps(incident *Incident) ([]*Step, error) {
	kind, ok, err := incident.Kind()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrIncidentKindUnknown
	}
	steps := []*Step{}
	for _, step := range self.Steps() {
		if step.SiteOnly && kind != IncidentKindSite {
			continue
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// Refuses step under incident unless it is the incident's to run and every step before it has ended.
func (self *Frame) checkRunnable(incident *Incident, step *Step) error {
	kind, ok, err := incident.Kind()
	if err != nil {
		return err
	}
	if !ok {
		return ErrIncidentKindUnknown
	}
	if step.SiteOnly && kind != IncidentKindSite {
		return fmt.Errorf("%w: %s in a %s incident", ErrStepSiteOnly, step.Name, kind)
	}
	steps, err := self.incidentSteps(incident)
	if err != nil {
		return err
	}
	for _, before := range steps {
		if before.Name == step.Name {
			return nil
		}
		if !incident.StepEnded(before.Name) {
			return fmt.Errorf("%w: %s before %s", ErrStepOutOfOrder, before.Name, step.Name)
		}
	}
	return fmt.Errorf("%w: %q", ErrStepUnknown, step.Name)
}

// Runs the step named name under incident, then records its end. A step that already ended runs again, changing
// nothing it changed, and its end is not recorded twice.
func (self *Frame) RunStep(ctx context.Context, incident *Incident, name string, out io.Writer, statement string) error {
	return self.RunStepWithOptions(ctx, incident, name, out, &StepOptions{Statement: statement})
}

// RunStep with the operator's inputs and apply as well as the statement. options may be nil for none.
func (self *Frame) RunStepWithOptions(ctx context.Context, incident *Incident, name string, out io.Writer, options *StepOptions) error {
	if options == nil {
		options = &StepOptions{}
	}
	step, err := self.step(name)
	if err != nil {
		return err
	}
	if err := self.checkRunnable(incident, step); err != nil {
		return err
	}
	if step.Function == nil {
		return fmt.Errorf("%w: %s; run it by hand, then state its end with sidecarctl sidecar restore end %s --by-hand=<how>", ErrStepUnregistered, step.Name, step.Name)
	}
	run := &StepRun{
		Incident:  incident,
		Out:       out,
		Statement: options.Statement,
		Inputs:    options.Inputs,
		Apply:     options.Apply,
	}
	if err := step.Function(ctx, run); err != nil {
		return fmt.Errorf("the %s step has not ended: %w", step.Name, err)
	}
	return incident.recordStepEnded(step.Name, self.now(), stepEndedRan)
}

// Runs every step of the incident that has not ended, in order, stopping at the first that fails or that no task has
// registered. The list must record the incident's kind (Incident.SetKind), which says whether the site steps run.
func (self *Frame) Run(ctx context.Context, incident *Incident, out io.Writer, statement string) error {
	return self.RunWithOptions(ctx, incident, out, &StepOptions{Statement: statement})
}

// Run with the operator's inputs and apply as well as the statement, each step given them all. options may be nil
// for none.
func (self *Frame) RunWithOptions(ctx context.Context, incident *Incident, out io.Writer, options *StepOptions) error {
	steps, err := self.incidentSteps(incident)
	if err != nil {
		return err
	}
	for _, step := range steps {
		if incident.StepEnded(step.Name) {
			continue
		}
		if err := self.RunStepWithOptions(ctx, incident, step.Name, out, options); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s ended\n", step.Name)
	}
	fmt.Fprintf(out, "every step of incident %s has ended\n", incident.IncidentId())
	return nil
}

// Records that the operator ran the step named name by hand, as statement says, once every step before it ended. Only
// a step no task has registered is ended this way: a registered step ends by running.
func (self *Frame) EndStepByHand(incident *Incident, name string, statement string) error {
	step, err := self.step(name)
	if err != nil {
		return err
	}
	if statement == "" {
		return errors.New("a step ended by hand needs the operator's statement of how it ran")
	}
	if step.Function != nil {
		return fmt.Errorf("the %s step is registered, so it ends by running: sidecarctl sidecar restore %s", step.Name, step.Name)
	}
	if err := self.checkRunnable(incident, step); err != nil {
		return err
	}
	return incident.recordStepEnded(step.Name, self.now(), stepEndedByHand+": "+statement)
}
