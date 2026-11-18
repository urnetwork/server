package work

// The observe soaks' report (D-TEST-12, D-TEST-18; EVS2-M10-33): how far format 2's two observe modes have come
// toward the evidence that lets an operator turn deletes on. It only reads: `purge: on` and `drop: on` stay an
// operator's act in §19.11's sidecar.yml, the report never touching a gate.
//
//	purge   D-TEST-12: evs's observe check (evsservice.PurgeObserveCheck) counted each candidate it held to every
//	        retained version that could name it, clean or retained, in urnetwork_evs_versionless_observe_total; the
//	        soak needs at least PurgeSoakDays since its first sample and PurgeSoakCandidates candidates checked, with
//	        none retained. A retained count above zero fails it whatever else holds, and a counter that fell between
//	        two samples (a restart) is read as having started again from zero there, never as negative. Each
//	        process pushes its own series, so the samples are read a series at a time (EvsObserveSample.Series): a
//	        sum over processes falls when one of them ends, by more or less than the others rose, which would
//	        count too many candidates or too few
//	drop    D-TEST-18: drop jobs that ran every phase under `drop: observe` and recorded their delete sets (state
//	        observed); the soak needs at least DropSoakDays since its first job and DropSoakJobs jobs, at least
//	        DropSoakRenameShare of them on repos with renames and cross-ref copies, and no job refused {verify}, the
//	        drop oracle's finding a delete-set member a retained version names. A job is on such a repo when its
//	        snap-0 file records both a rename and a cross-ref copy among its listed versions (owner decision 513)
//
// The inputs are what the operations command reads for it (`sidecarctl evs observe-report <env>`, owner decision 513,
// QD-987): the counter's samples from the monitor and each repo's drop table rows and its jobs' snap-0 files
// (question evs2-m10-33-observe-report-command). Every count is of opaque events; nothing here names a network, repo
// or path (I-13).

import (
	"fmt"
	"slices"
	"time"
)

// The soaks' thresholds, D-TEST-12's and D-TEST-18's.
type EvsObserveSoakSettings struct {
	// D-TEST-12: at least 14 days and 10,000 candidates, none retained.
	PurgeSoakDays       int
	PurgeSoakCandidates int64
	// D-TEST-18: at least 30 days and 200 jobs, half on repos with renames and cross-ref copies.
	DropSoakDays        int
	DropSoakJobs        int
	DropSoakRenameShare float64
}

func DefaultEvsObserveSoakSettings() *EvsObserveSoakSettings {
	return &EvsObserveSoakSettings{
		PurgeSoakDays:       14,
		PurgeSoakCandidates: 10000,
		DropSoakDays:        30,
		DropSoakJobs:        200,
		DropSoakRenameShare: 0.5,
	}
}

// One reading of urnetwork_evs_versionless_observe_total.
type EvsObserveSample struct {
	At time.Time
	// The series read, as the monitor tells the processes that push the counter apart: a fall is a restart of that
	// series alone. Opaque, compared and never printed. "" for a reading summed over the processes.
	Series   string
	Clean    int64
	Retained int64
}

// One drop job as the soak counts it.
type EvsObserveDropJob struct {
	EndedAt time.Time
	// Ran every phase and recorded its delete set under drop: observe.
	Observed bool
	// Refused {verify}: the drop oracle found a delete-set member a retained version names.
	RefusedVerify bool
	// What its snap-0 records of its listed versions (owner decision 513): a push that removed one path and created
	// another citing its kind-5 content, and a chain that named a reuse source of another ref. The job counts toward
	// the share when both hold.
	Renames        bool
	CrossRefCopies bool
}

// What the report says of one soak.
type EvsObserveSoak struct {
	Start time.Time
	Days  float64
	Met   bool
	// Why it is not met, one line a threshold.
	Unmet []string
}

// The purge soak.
type EvsObservePurgeSoak struct {
	EvsObserveSoak
	Candidates int64
	Retained   int64
}

// The drop soak.
type EvsObserveDropSoak struct {
	EvsObserveSoak
	Jobs int
	// Observed jobs whose snap-0 records both a rename and a cross-ref copy, which the share counts, and those that
	// record each.
	RenamesAndCopies int
	Renames          int
	CrossRefCopies   int
	RefusedVerify    int
}

// Both soaks at now.
type EvsObserveSoakReport struct {
	At    time.Time
	Purge *EvsObservePurgeSoak
	Drop  *EvsObserveDropSoak
}

// The report over the counter's samples and the drop jobs, at now. See the file header.
func EvsObserveSoakAt(samples []*EvsObserveSample, jobs []*EvsObserveDropJob, now time.Time, settings *EvsObserveSoakSettings) *EvsObserveSoakReport {
	if settings == nil {
		settings = DefaultEvsObserveSoakSettings()
	}
	return &EvsObserveSoakReport{At: now, Purge: evsObservePurgeSoak(samples, now, settings), Drop: evsObserveDropSoak(jobs, now, settings)}
}

// D-TEST-12's soak from the counter's samples.
func evsObservePurgeSoak(samples []*EvsObserveSample, now time.Time, settings *EvsObserveSoakSettings) *EvsObservePurgeSoak {
	soak := &EvsObservePurgeSoak{}
	ordered := slices.Clone(samples)
	slices.SortStableFunc(ordered, func(a *EvsObserveSample, b *EvsObserveSample) int { return a.At.Compare(b.At) })
	// each series' reading before the one at hand
	seriesPriors := map[string]*EvsObserveSample{}
	for _, sample := range ordered {
		if soak.Start.IsZero() || sample.At.Before(soak.Start) {
			soak.Start = sample.At
		}
		prior := seriesPriors[sample.Series]
		seriesPriors[sample.Series] = sample
		if prior == nil {
			// a series' first reading counts whole: a process starts its counter at zero
			soak.Candidates += sample.Clean + sample.Retained
			soak.Retained += sample.Retained
			continue
		}
		clean, retained := sample.Clean-prior.Clean, sample.Retained-prior.Retained
		if clean < 0 || retained < 0 {
			// a counter that fell restarted from zero at this sample
			clean, retained = sample.Clean, sample.Retained
		}
		soak.Candidates += clean + retained
		soak.Retained += retained
	}
	if !soak.Start.IsZero() {
		soak.Days = now.Sub(soak.Start).Hours() / 24
	}
	if soak.Start.IsZero() {
		soak.Unmet = append(soak.Unmet, "no observe check has been counted")
	}
	if soak.Days < float64(settings.PurgeSoakDays) {
		soak.Unmet = append(soak.Unmet, fmt.Sprintf("%.1f of %d days", soak.Days, settings.PurgeSoakDays))
	}
	if soak.Candidates < settings.PurgeSoakCandidates {
		soak.Unmet = append(soak.Unmet, fmt.Sprintf("%d of %d candidates checked", soak.Candidates, settings.PurgeSoakCandidates))
	}
	if 0 < soak.Retained {
		soak.Unmet = append(soak.Unmet, fmt.Sprintf("%d candidates a retained version names", soak.Retained))
	}
	soak.Met = len(soak.Unmet) == 0
	return soak
}

// D-TEST-18's soak from the drop jobs.
func evsObserveDropSoak(jobs []*EvsObserveDropJob, now time.Time, settings *EvsObserveSoakSettings) *EvsObserveDropSoak {
	soak := &EvsObserveDropSoak{}
	for _, job := range jobs {
		if job.RefusedVerify {
			soak.RefusedVerify += 1
			continue
		}
		if !job.Observed {
			continue
		}
		soak.Jobs += 1
		if job.Renames {
			soak.Renames += 1
		}
		if job.CrossRefCopies {
			soak.CrossRefCopies += 1
		}
		if job.Renames && job.CrossRefCopies {
			soak.RenamesAndCopies += 1
		}
		if soak.Start.IsZero() || job.EndedAt.Before(soak.Start) {
			soak.Start = job.EndedAt
		}
	}
	if !soak.Start.IsZero() {
		soak.Days = now.Sub(soak.Start).Hours() / 24
	}
	if soak.Jobs == 0 {
		soak.Unmet = append(soak.Unmet, "no observed drop job")
	}
	if soak.Days < float64(settings.DropSoakDays) {
		soak.Unmet = append(soak.Unmet, fmt.Sprintf("%.1f of %d days", soak.Days, settings.DropSoakDays))
	}
	if soak.Jobs < settings.DropSoakJobs {
		soak.Unmet = append(soak.Unmet, fmt.Sprintf("%d of %d jobs", soak.Jobs, settings.DropSoakJobs))
	}
	if float64(soak.RenamesAndCopies) < settings.DropSoakRenameShare*float64(max(soak.Jobs, settings.DropSoakJobs)) {
		soak.Unmet = append(soak.Unmet, fmt.Sprintf("%d jobs on repos with renames and cross-ref copies, under %.0f%%", soak.RenamesAndCopies, 100*settings.DropSoakRenameShare))
	}
	if 0 < soak.RefusedVerify {
		soak.Unmet = append(soak.Unmet, fmt.Sprintf("%d jobs the drop oracle refused", soak.RefusedVerify))
	}
	soak.Met = len(soak.Unmet) == 0
	return soak
}

// The report as lines for a terminal: each soak's counts and, when unmet, why.
func (self *EvsObserveSoakReport) Lines() []string {
	verdict := func(met bool) string {
		if met {
			return "met"
		}
		return "not met"
	}
	lines := []string{
		fmt.Sprintf("purge observe (D-TEST-12): %s: %.1f days, %d candidates checked, %d retained", verdict(self.Purge.Met), self.Purge.Days, self.Purge.Candidates, self.Purge.Retained),
	}
	for _, unmet := range self.Purge.Unmet {
		lines = append(lines, "  "+unmet)
	}
	lines = append(lines, fmt.Sprintf("drop observe (D-TEST-18): %s: %.1f days, %d jobs, %d on repos with renames and cross-ref copies (%d with renames, %d with cross-ref copies), %d refused by the oracle",
		verdict(self.Drop.Met), self.Drop.Days, self.Drop.Jobs, self.Drop.RenamesAndCopies, self.Drop.Renames, self.Drop.CrossRefCopies, self.Drop.RefusedVerify))
	for _, unmet := range self.Drop.Unmet {
		lines = append(lines, "  "+unmet)
	}
	return lines
}
