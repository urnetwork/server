package controller

// What the services a process runs run on, as parts installed process-wide, each once. A part is one install that a
// service's routes or tasks read from: a value set in this process (SetAbuseEffects, SetAclContractGate and their
// kind), and for some a worker of its own that keeps it current (§21's standing snapshot, the notice pager's retry
// sink, the projection's gate cache, the issuer's task worker). Each service names what it runs on as a list of
// parts: api's route set (ApiRoutesInstallParts), connect's exchange (ConnectExchangeInstallParts) and taskworker's
// tasks (work.TaskworkerTasksInstallParts). A process that runs one service installs its list through that service's
// install (InstallApiRoutes, InstallConnectExchange, work.InstallTaskworkerTasks); all's, which runs the three in one
// process, installs their lists as one through InstallProcessParts, so that:
//
//   - a part two services name is installed once. api and taskworker both run on §21's effects, record mailers,
//     notice pager and standing (AbuseInstallParts), so a process that runs both runs one notice pager's sink and
//     one standing snapshot, where installing both lists whole would run two of each and leave each value set to
//     whichever install came second;
//   - a part that covers another service's part stands in for it. api's standing, whose snapshot is the process's
//     §21 hard deny, covers connect's hard deny; api's authorization, whose projection installs the contract gate
//     over api's own gate cache, read from the first metadata cluster, covers connect's, which follows api's
//     internal gate surface from a process that holds no metadata credentials. A covered part is not installed in a
//     process whose lists hold its cover, so the process runs one snapshot and one gate cache, and the gate its
//     residents read is not replaced by a second.
//
// The issuer's task worker, which api's authorization runs, claims only the issuer's targets, which taskworker's
// worker leaves (taskworker/claim_admission.go), so the two workers of a process that runs both never claim the
// same task.
//
// Parts are installed in list order, the first of each name, and removed in the reverse, each before the next.

import (
	"context"
	"fmt"
	"slices"
)

// Names a part: an identifier, the same in every list that names the part.
type ProcessInstallPartName string

// §21's parts, which api's route set and taskworker's tasks both run on (AbuseInstallParts).
const (
	InstallPartAbuseEffects          ProcessInstallPartName = "abuse-effects"
	InstallPartAbuseRecordMailer     ProcessInstallPartName = "abuse-record-mailer"
	InstallPartLegalHoldRecordMailer ProcessInstallPartName = "legal-hold-record-mailer"
	InstallPartAbuseNoticePager      ProcessInstallPartName = "abuse-notice-pager"
	InstallPartAbuseStanding         ProcessInstallPartName = "abuse-standing"
)

// One part of what a process's services run on (the file header).
type ProcessInstallPart struct {
	Name ProcessInstallPartName
	// The parts of other services' lists this one stands in for in a process that installs it. A part that covers
	// others is never itself covered.
	Covers []ProcessInstallPartName
	// Installs the part on ctx and answers what removes it again.
	Install func(ctx context.Context) (remove func())
}

// The parts of parts that InstallProcessParts installs, in order: the first of each name that no part of parts
// covers. Panics where a part that covers others is itself covered, a list no process can install whole, since the
// cover would be left out with what it covers.
func SelectProcessInstallParts(parts []ProcessInstallPart) []ProcessInstallPart {
	coveredNames := map[ProcessInstallPartName]bool{}
	for _, part := range parts {
		for _, name := range part.Covers {
			coveredNames[name] = true
		}
	}
	selectedNames := map[ProcessInstallPartName]bool{}
	selected := []ProcessInstallPart{}
	for _, part := range parts {
		if coveredNames[part.Name] {
			if 0 < len(part.Covers) {
				panic(fmt.Errorf("the install part %s covers others and is itself covered", part.Name))
			}
			continue
		}
		if selectedNames[part.Name] {
			continue
		}
		selectedNames[part.Name] = true
		selected = append(selected, part)
	}
	return selected
}

// Installs parts on ctx as the file header says, and answers what removes them again, the last installed first,
// each removed before the next.
func InstallProcessParts(ctx context.Context, parts []ProcessInstallPart) func() {
	removes := []func(){}
	for _, part := range SelectProcessInstallParts(parts) {
		removes = append(removes, part.Install(ctx))
	}
	return func() {
		for _, remove := range slices.Backward(removes) {
			remove()
		}
	}
}

// What §21 runs on in each process that runs its routes or its passes, api's and taskworker's, in their order:
//
//	effects          the effects of the actions this process applies (InstallAbuseEffects; ABUSE-M5-06)
//	record mailers   the sealed off-site record of each action and each hold (InstallAbuseRecordMailer,
//	                 InstallLegalHoldRecordMailer; QD-726)
//	notice pager     §21.8's pages through the monitor's webhook (InstallAbuseNoticePager; QD-1070)
//	standing         §21.7's standing and network_verified, and its snapshot as the hard deny CreateContract reads,
//	                 which covers connect's hard deny (InstallAbuseStanding; ABUSE-M5-07)
func AbuseInstallParts() []ProcessInstallPart {
	return []ProcessInstallPart{
		{Name: InstallPartAbuseEffects, Install: func(context.Context) func() {
			return InstallAbuseEffects()
		}},
		{Name: InstallPartAbuseRecordMailer, Install: func(context.Context) func() {
			return InstallAbuseRecordMailer()
		}},
		{Name: InstallPartLegalHoldRecordMailer, Install: func(context.Context) func() {
			return InstallLegalHoldRecordMailer()
		}},
		{Name: InstallPartAbuseNoticePager, Install: func(context.Context) func() {
			return InstallAbuseNoticePager()
		}},
		{Name: InstallPartAbuseStanding, Covers: []ProcessInstallPartName{InstallPartAbuseHardDeny}, Install: InstallAbuseStanding},
	}
}
