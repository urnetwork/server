//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The gkm concurrency case's two cases beside the churn (gkm_concurrency_test.go), each on a world of its own with
// the recorder at gkm's origin and §10.6's reconciler (gkm_concurrency_harness_test.go), checked as the churn is:
//
//   - a re-form under contention, on two general repos every member reads:
//       - the contended repo: two devices re-form its group in full at once (GkmReformArgs), while a third rotates it
//         and the owner writes it, all released together; gkm takes one form, and the other re-former's loses its
//         place and answers ErrGkmWait, adopting the group that stands at its next sync (gkm_reform.go's postReform)
//       - the resumed repo: one device's re-form is cut short at its ladder, the form standing and the keychain's
//         record left at its ladder phase; then the device asks again with GkmReformArgs.Reseed, which carries the
//         record through the ladder and stops before the rotation, as §19.10's groups step asks it first, while the
//         rotator rotates the repo; and then with GkmReformArgs.Resume, the step's second call, which rotates into the
//         new group, or answers the group a sync finished first (QD-1143)
//     For each: one re-form of the old group stands; no generation stands in the old group past the form's; the new
//     group's ladder is the old one's GenRotates byte for byte; its newest generation is past the old group's and names
//     the new group; every member converges on it with no fork; and every member's write then reads on every other
//     member, through evs. A Reseed with no record, on a live repo whose newest form names the group the device pins,
//     re-forms nothing and answers that group (QD-1143), so the reseed is asked of a record a cut-short re-form left
//   - a personal network's new second device: a network of one device writes a general repo, a group inbox and a
//     vault, and nothing more happens to them; then a second device enrolls and is admitted, and must read every file
//     of every repo, holding each repo's one generation, by the first wrap its admission's signer writes or by its own
//     late ask: an Add keeps the generation (§10.5), so nothing may rotate
//
// A failure from a product bug stays failing; each case reports it with the recorder's evidence. As they landed
// (2026-11-22) both fail, on these, every other check passing:
//
//   - the contended repo, in some runs: gkm takes a rotation of the old group made after the new form stands, so the
//     repo holds a generation in each group at one number; the rotator pins the old group's, refuses the new group by
//     §10.8's members row (its pin never falls) with gkm refused alarms at every resync and evs tamper alarms, and its
//     writes are refused 412 GATE_CLOSED. §10.6's check reads the forms and then creates gen/<g> under the old
//     group's prefix with nothing conditioning both; QD-1186 (question e2e-gkm-concurrency-reform-old-group-claim)
//     decided gkm fences the old group at a re-form. In the other runs gkm refuses the claim 403, which the case
//     accounts for
//   - both repos: the re-former's first generation in the new group is staged to every holder but the owner's device,
//     the network's first, which has published no ReaderKey yet: its upkeep's first pass ran before it held any repo
//     scope ("gkm: no repo scope to present") and nothing runs it again before its first resync. Its late ask, which
//     publishes the key first (gkm-pending-hint-delay), is then answered by no holder within the bound: no delivery
//     to it of the new group's generation is ever made, so it reads and writes nothing sealed past the re-form
//   - the resumed repo: after the reseed and the resume, evs refuses every member's write 503 EVS_REFUSAL_UNAVAILABLE
//     (gkm_reform.go's header leaves evs's resolution of gen/<g> through the newest GroupForm to evs)
//   - the second device: api's token exchange refuses every scope its key client asks for the vault ("the projection
//     grants none of the scopes", 400 invalid_scope), though §8.5's scoped clause makes it a consented device of the
//     vault's member (the owner's genesis device signed its admission) and its own verified state gives it a role
//     there, so it never takes the vault's key; the general repo and the group inbox each reach it by one LATE
//     delivery its admission's signer makes at its first late ask
//
// Each runs inside the conventions' 10 minutes. Run them through the build's wrapper: -tags 'sidecar_e2e
// sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server/sidecart3"
)

// What the re-form case runs with (QD-702), beside the churn's timings and bounds.
type gkmConcurrencyReformSettings struct {
	// The churn's settings: its gkm timings, bounds, polls and syncs.
	Case *gkmConcurrencySettings
	// The sidecars of the network: the owner, the two re-formers and the rotator.
	ClientCount int
	// The rotations of each repo before its re-form, so its ladder carries more than generation 1.
	RotationCount int
}

func defaultGkmConcurrencyReformSettings() *gkmConcurrencyReformSettings {
	return &gkmConcurrencyReformSettings{
		Case:          defaultGkmConcurrencySettings(),
		ClientCount:   4,
		RotationCount: 1,
	}
}

// What the second device's case runs with (QD-702), beside the churn's timings and bounds.
type gkmConcurrencySecondDeviceSettings struct {
	// The churn's settings: its gkm timings, bounds, polls and syncs.
	Case *gkmConcurrencySettings
	// The files the first device writes in each repo before the second enrolls.
	FileCount int
}

func defaultGkmConcurrencySecondDeviceSettings() *gkmConcurrencySecondDeviceSettings {
	return &gkmConcurrencySecondDeviceSettings{
		Case:      defaultGkmConcurrencySettings(),
		FileCount: 2,
	}
}

// A world of one network of clientCount sidecars under the implicit document, the recorder at gkm's origin before any
// device, and the reconciler: the case beside the churn, its devices and config repo added. Closed at t's end.
func startGkmConcurrencySideCase(t testing.TB, settings *gkmConcurrencySettings, clientCount int) *gkmConcurrencyCase {
	worldSettings := DefaultWorldSettings()
	worldSettings.NetworkCount = 1
	worldSettings.ClientsPerNetwork = clientCount
	t3Settings := sidecart3.DefaultT3WorldSettings()
	limits := *t3Settings.SidecarLimits
	limits.LimitKeyValues = maps.Clone(limits.LimitKeyValues)
	limits.LimitKeyValues["config_bases_per_min"] = settings.ConfigBasesPerMinute
	t3Settings.SidecarLimits = &limits
	worldSettings.T3 = t3Settings
	world := NewWorld(t, worldSettings)
	origin, err := installGkmConcurrencyOrigin(world)
	if err != nil {
		t.Fatal(err)
	}
	reconcilerSettings := gkmservice.DefaultGkmReconcilerSettings()
	reconcilerSettings.PendingHintInterval = settings.ReconcilerHintInterval
	reconcilerSettings.DroppedLeafHintInterval = settings.ReconcilerHintInterval
	reconciler, err := startGkmConcurrencyReconciler(world, reconcilerSettings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reconciler.close)
	self := &gkmConcurrencyCase{t: t, settings: settings, world: world, origin: origin, reconciler: reconciler, refusalAccounts: map[string]int{}, retryCounts: map[string]int{}}
	origin.setDescribe(self.describeCaller)
	t.Cleanup(func() {
		for _, device := range self.deviceList() {
			for _, remove := range device.removes {
				remove()
			}
		}
	})
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	network, err := world.AddNetwork(ctx, worldNetworkName(0), clientCount, settings.sidecarSettings)
	if err != nil {
		world.FailProgress(t, "the network", err, nil)
	}
	self.network = network
	for _, client := range network.Clients {
		if _, err := self.addDevice(client, false); err != nil {
			t.Fatal(err)
		}
	}
	self.owner = self.deviceList()[0]
	configRepoId, err := gkmConcurrencyConfigRepoId(self.owner.device)
	if err != nil {
		t.Fatal(err)
	}
	self.config = &gkmConcurrencyRepo{name: "config", repoId: configRepoId, config: true}
	return self
}

// A repo the owner creates as args asks, which every member device reads from its creation on: a general repo under
// the implicit document's member admin, or the member's owner-bound one.
func (self *gkmConcurrencyCase) createOtherRepo(ctx context.Context, name string, args *sidecar.EvsCreateRepoArgs) (*gkmConcurrencyRepo, error) {
	client, err := self.owner.device.EvsClient()
	if err != nil {
		return nil, err
	}
	created, err := client.CreateRepo(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("the %s repo: %w", name, err)
	}
	repo := &gkmConcurrencyRepo{name: name, repoId: created.RepoId(), admin: self.owner}
	// its RepoCreate is the newest entry the owner holds once the creation's sync returned
	repo.memberGrantAt = gkmConcurrencyStateVersion(self.owner.device)
	self.others = append(self.others, repo)
	return repo, nil
}

// See the file's header.
func TestE2eGkmConcurrencyReform(t *testing.T) {
	settings := defaultGkmConcurrencyReformSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		runGkmConcurrencyReform(t, settings)
	})
}

// The re-form case on settings: see the file's header.
func runGkmConcurrencyReform(t testing.TB, settings *gkmConcurrencyReformSettings) {
	if settings.ClientCount < 4 {
		t.Fatalf("a re-form case of %d sidecars has no rotator beside its two re-formers", settings.ClientCount)
	}
	self := startGkmConcurrencySideCase(t, settings.Case, settings.ClientCount)
	defer self.world.Close()
	caseSettings := settings.Case
	ctx, cancel := self.world.StepContext(t.Context(), caseSettings.PhaseTimeout)
	defer cancel()
	devices := self.deviceList()
	owner, first, second, rotator := devices[0], devices[1], devices[2], devices[3]
	contended, err := self.createOtherRepo(ctx, "contended", &sidecar.EvsCreateRepoArgs{Kind: protocol.RepoKind_REPO_KIND_GENERAL, Confirmed: true})
	if err != nil {
		self.world.FailProgress(t, "the contended repo", err, nil)
	}
	resumed, err := self.createOtherRepo(ctx, "resumed", &sidecar.EvsCreateRepoArgs{Kind: protocol.RepoKind_REPO_KIND_GENERAL, Confirmed: true})
	if err != nil {
		self.world.FailProgress(t, "the resumed repo", err, nil)
	}
	repos := []*gkmConcurrencyRepo{contended, resumed}
	for _, repo := range repos {
		for _, device := range devices {
			if err := self.write(ctx, device, repo, fmt.Sprintf("before-%s.txt", device.name)); err != nil {
				self.world.FailProgress(t, "the writes before the re-forms", err, nil)
			}
		}
		for index := range settings.RotationCount {
			if _, err := owner.device.Gkm.RotateRepo(ctx, repo.repoId); err != nil {
				self.world.FailProgress(t, fmt.Sprintf("%s's rotation %d before the re-forms", repo.name, index), err, nil)
			}
		}
	}
	if err := self.awaitConvergence(ctx, "before the re-forms", caseSettings.convergenceBound(false)); err != nil {
		self.world.FailProgress(t, "before the re-forms", err, nil)
	}
	oldGroupIds := map[*gkmConcurrencyRepo]sidecar.Id{}
	oldNewest := map[*gkmConcurrencyRepo]*gkmConcurrencyGeneration{}
	oldLadders := map[*gkmConcurrencyRepo]map[uint64]*gkmConcurrencyGeneration{}
	generations, _ := gkmConcurrencyGenerations(self.origin.claimList(), self.origin.formList())
	for _, repo := range repos {
		oldGroupIds[repo], _ = self.repoGroup(repo)
		oldNewest[repo] = self.newestGeneration(repo)
		oldLadders[repo] = maps.Clone(generations[oldGroupIds[repo]])
	}
	self.accountRefusals("before the re-forms")
	reform := func(device *gkmConcurrencyDevice, args *sidecar.GkmReformArgs) (sidecar.Id, error) {
		reformer, ok := device.device.Gkm.(gkmConcurrencyReformer)
		if !ok {
			return sidecar.Id{}, fmt.Errorf("%s's key client re-forms nothing", device.name)
		}
		return reformer.ReformGroup(ctx, args)
	}
	// a re-former's answer: the group it formed, or the loser's ErrGkmWait, another re-form standing
	answers := map[*gkmConcurrencyRepo]map[string]sidecar.Id{contended: {}, resumed: {}}

	// the contended repo: two full re-forms, a rotation of the group they re-form and a write, released together
	release := make(chan struct{})
	var group sync.WaitGroup
	var firstGroupId, secondGroupId sidecar.Id
	var firstErr, secondErr, rotateErr, writeErr error
	var rotated *sidecar.EvsRepoKey
	group.Go(func() {
		<-release
		firstGroupId, firstErr = reform(first, &sidecar.GkmReformArgs{RepoId: contended.repoId})
	})
	group.Go(func() {
		<-release
		secondGroupId, secondErr = reform(second, &sidecar.GkmReformArgs{RepoId: contended.repoId})
	})
	group.Go(func() {
		<-release
		rotated, rotateErr = rotator.device.Gkm.RotateRepo(ctx, contended.repoId)
	})
	group.Go(func() {
		<-release
		writeErr = self.write(ctx, owner, contended, "during-owner.txt")
	})
	close(release)
	group.Wait()
	t.Logf("the contended repo: %s's re-form answered group %s (%v), %s's group %s (%v); %s's rotation %s (%v); the owner's write %v", first.name, firstGroupId, firstErr, second.name, secondGroupId, secondErr, rotator.name, describeGkmConcurrencyKey(rotated), rotateErr, writeErr)
	for device, outcome := range map[*gkmConcurrencyDevice]struct {
		groupId sidecar.Id
		err     error
	}{first: {groupId: firstGroupId, err: firstErr}, second: {groupId: secondGroupId, err: secondErr}} {
		switch {
		case outcome.err == nil:
			answers[contended][device.name] = outcome.groupId
		case errors.Is(outcome.err, sidecar.ErrGkmWait):
			// the loser: another re-form stands, which it adopts at its next sync
		default:
			t.Errorf("the contended repo: %s's re-form: %v", device.name, outcome.err)
		}
	}
	if len(answers[contended]) != 1 {
		t.Errorf("the contended repo: %d re-forms answered as standing, not one", len(answers[contended]))
	}

	// the resumed repo: a re-form cut short at its ladder, the rotator racing it; then its reseed, which carries the
	// record through the ladder and stops, and its resume, which rotates into the new group
	ladderCut, removeLadderCut := self.origin.cut("the re-former's ladder", func(request *gkmConcurrencyRequest) bool {
		return request.kind == gkmConcurrencyRequestLadder && request.sidecarId == first.sidecarId
	})
	_, cutErr := reform(first, &sidecar.GkmReformArgs{RepoId: resumed.repoId})
	removeLadderCut()
	t.Logf("the resumed repo: %s's re-form cut at its ladder (%d calls lost) answered %v", first.name, self.origin.cutCount(ladderCut), cutErr)
	if cutErr == nil || self.origin.cutCount(ladderCut) == 0 {
		t.Errorf("the resumed repo: %s's re-form answered %v with %d ladder calls lost, so no record was left at its ladder", first.name, cutErr, self.origin.cutCount(ladderCut))
	}
	release = make(chan struct{})
	var reseedGroupId sidecar.Id
	var reseedErr error
	group.Go(func() {
		<-release
		reseedGroupId, reseedErr = reform(first, &sidecar.GkmReformArgs{RepoId: resumed.repoId, Reseed: true})
	})
	group.Go(func() {
		<-release
		rotated, rotateErr = rotator.device.Gkm.RotateRepo(ctx, resumed.repoId)
	})
	close(release)
	group.Wait()
	resumedGroupId, resumeErr := reform(first, &sidecar.GkmReformArgs{RepoId: resumed.repoId, Resume: true})
	t.Logf("the resumed repo: %s's reseed answered group %s (%v), its resume group %s (%v); %s's rotation %s (%v)", first.name, reseedGroupId, reseedErr, resumedGroupId, resumeErr, rotator.name, describeGkmConcurrencyKey(rotated), rotateErr)
	switch {
	case reseedErr != nil:
		t.Errorf("the resumed repo: %s's reseed of its record: %v", first.name, reseedErr)
	case resumeErr != nil && !errors.Is(resumeErr, sidecar.ErrGkmKeyMissing):
		// ErrGkmKeyMissing: a sync finished the record first, as QD-1143 lets it
		t.Errorf("the resumed repo: %s's resume of its record: %v", first.name, resumeErr)
	case resumeErr == nil && resumedGroupId != reseedGroupId:
		t.Errorf("the resumed repo: %s's resume answered group %s, its reseed %s", first.name, resumedGroupId, reseedGroupId)
	default:
		answers[resumed][first.name] = reseedGroupId
	}

	// a re-form's first generation past the ladder may need a sync to land, which a lost hint delays to a resync
	if err := self.awaitConvergence(ctx, "after the re-forms", caseSettings.convergenceBound(true)); err != nil {
		t.Errorf("%v", err)
		for _, repo := range repos {
			self.logReformEvidence(repo, oldGroupIds[repo])
		}
	}
	self.accountRefusals("the re-forms")

	generations, forks := gkmConcurrencyGenerations(self.origin.claimList(), self.origin.formList())
	for _, fork := range forks {
		t.Errorf("a fork at gkm: %s", fork)
	}
	for _, repo := range repos {
		// gkm took one re-form of the old group, and the forms chain from it
		newGroupId, _ := self.repoGroup(repo)
		takenForms := []*gkmConcurrencyForm{}
		for _, form := range self.origin.formList() {
			if !form.ladder && form.taken() && form.repoId == repo.repoId {
				takenForms = append(takenForms, form)
			}
		}
		if len(takenForms) != 1 || newGroupId == oldGroupIds[repo] {
			t.Errorf("%s: gkm took %d re-forms of its group %s, not one", repo.name, len(takenForms), oldGroupIds[repo])
		}
		previous := oldGroupIds[repo]
		for _, form := range takenForms {
			if !bytes.Equal(form.form.GetPreviousGroupId(), previous.Bytes()) {
				t.Errorf("%s: gkm took a form of group %s re-forming %x, not the repo's group then, %s", repo.name, form.groupId(), form.form.GetPreviousGroupId(), previous)
			}
			previous = form.groupId()
		}
		for name, groupId := range answers[repo] {
			if groupId != newGroupId {
				t.Errorf("%s: %s's re-form answered group %s, not the group that stands, %s", repo.name, name, groupId, newGroupId)
			}
		}
		// no generation stands in the old group past the ladder the form carried
		formGeneration := uint64(0)
		if 0 < len(takenForms) {
			formGeneration = takenForms[0].form.GetGeneration()
		}
		for g, generation := range generations[oldGroupIds[repo]] {
			if formGeneration < g {
				t.Errorf("%s: gkm took generation %d in the old group %s, past the form's generation %d, by %s's claim at %s: a rotation of the group the form re-forms, which no member of the new group follows", repo.name, g, oldGroupIds[repo], formGeneration, self.nameOf(generation.signerId), generation.time.Format("15:04:05.000"))
			}
		}
		// the new group's ladder is the old one's, byte for byte, through the form's generation
		for g, old := range oldLadders[repo] {
			if formGeneration < g {
				continue
			}
			carried := generations[newGroupId][g]
			if carried == nil || !carried.ladder || !bytes.Equal(carried.rotate.GetCommitment(), old.rotate.GetCommitment()) || !bytes.Equal(carried.rotate.GetGroupId(), old.rotate.GetGroupId()) {
				t.Errorf("%s: the new group %s does not carry the old group's generation %d byte for byte", repo.name, newGroupId, g)
			}
		}
		// and its newest generation is past the old group's, claimed in the new group
		if newest := self.newestGeneration(repo); newest == nil || oldNewest[repo] == nil || newest.rotate.GetGeneration() <= oldNewest[repo].rotate.GetGeneration() || !bytes.Equal(newest.rotate.GetGroupId(), newGroupId.Bytes()) {
			t.Errorf("%s's newest generation is %s, not one past the old group's %s claimed in the new group %s", repo.name, describeGkmConcurrencyGeneration(newest), describeGkmConcurrencyGeneration(oldNewest[repo]), newGroupId)
		}
	}

	// every member's write under the newest generation reads on every other member
	for _, repo := range repos {
		for _, device := range devices {
			if err := self.write(ctx, device, repo, fmt.Sprintf("after-%s.txt", device.name)); err != nil {
				t.Errorf("after the re-forms: %v", err)
			}
		}
		for _, reader := range devices {
			for _, writer := range devices {
				path := fmt.Sprintf("after-%s.txt", writer.name)
				if err := worldKeysRead(ctx, reader.device, repo.repoId, path, gkmConcurrencyContent(repo, path)); err != nil {
					t.Errorf("after the re-forms: %s: %v", repo.name, err)
				}
			}
		}
	}
	self.accountRefusals("the writes after the re-forms")
	self.checkGenerations()
	self.checkDeliveries()
	self.checkKeychains()
	self.checkAlarms()
	self.logSummary()
}

// What the recorder saw of repo's two groups, for a re-form that did not converge: every claim gkm was asked to take
// in either, each delivery of their generations past the old group's ladder with gkm's answer, and every late ask.
func (self *gkmConcurrencyCase) logReformEvidence(repo *gkmConcurrencyRepo, oldGroupId sidecar.Id) {
	newGroupId, _ := self.repoGroup(repo)
	groupName := func(groupId sidecar.Id) string {
		switch groupId {
		case oldGroupId:
			return "the old group"
		case newGroupId:
			return "the new group"
		}
		return "group " + groupId.String()
	}
	for _, claim := range self.origin.claimList() {
		if claim.repoId != repo.repoId || claim.rotate == nil {
			continue
		}
		self.t.Logf("%s: %s's claim of %s's generation %d (cause %d, auth %d) at %s: gkm answered %d, lost %t", repo.name, self.nameOf(claim.uploader), groupName(claim.groupId), claim.rotate.GetGeneration(), claim.rotate.GetCause(), claim.rotate.GetAuth().GetVersion(), claim.time.Format("15:04:05.000"), claim.status, claim.lost)
	}
	for _, form := range self.origin.formList() {
		if form.repoId != repo.repoId && !(form.ladder && (form.groupId() == newGroupId || form.groupId() == oldGroupId)) {
			continue
		}
		what := fmt.Sprintf("form of %s re-forming %x at generation %d", groupName(form.groupId()), form.form.GetPreviousGroupId(), form.form.GetGeneration())
		if form.ladder {
			what = fmt.Sprintf("ladder call of %d generations into %s", len(form.generations), groupName(form.groupId()))
		}
		self.t.Logf("%s: %s's %s at %s: gkm answered %d", repo.name, self.nameOf(form.uploader), what, form.time.Format("15:04:05.000"), form.status)
	}
	for _, delivery := range self.origin.deliveryList() {
		if delivery.groupId != oldGroupId && delivery.groupId != newGroupId || delivery.body == nil {
			continue
		}
		self.t.Logf("%s: %s's delivery of %s's generation %d (kind %d) to %s at %s: gkm answered %d", repo.name, self.nameOf(delivery.uploader), groupName(delivery.groupId), delivery.generation, delivery.body.GetDeliveryKind(), self.nameOf(delivery.recipient), delivery.time.Format("15:04:05.000"), delivery.status)
	}
	for _, ask := range self.origin.askList() {
		if ask.groupId != oldGroupId && ask.groupId != newGroupId {
			continue
		}
		self.t.Logf("%s: %s's late ask of %s's generation %d at %s: gkm answered %d", repo.name, self.nameOf(ask.sidecarId), groupName(ask.groupId), ask.generation, ask.time.Format("15:04:05.000"), ask.status)
	}
}

// The group a form forms.
func (self *gkmConcurrencyForm) groupId() sidecar.Id {
	groupId, _ := sidecar.IdFromBytes(self.form.GetGroupId())
	return groupId
}

// A key for the case's lines.
func describeGkmConcurrencyKey(key *sidecar.EvsRepoKey) string {
	if key == nil {
		return "no key"
	}
	return fmt.Sprintf("generation %d of group %s", key.Generation(), key.GroupId())
}

// A generation for the case's lines.
func describeGkmConcurrencyGeneration(generation *gkmConcurrencyGeneration) string {
	if generation == nil {
		return "none"
	}
	return fmt.Sprintf("generation %d of group %s (claimed in %x)", generation.rotate.GetGeneration(), generation.groupId, generation.rotate.GetGroupId())
}

// See the file's header.
func TestE2eGkmConcurrencySecondDevice(t *testing.T) {
	settings := defaultGkmConcurrencySecondDeviceSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		runGkmConcurrencySecondDevice(t, settings)
	})
}

// The second device's case on settings: see the file's header.
func runGkmConcurrencySecondDevice(t testing.TB, settings *gkmConcurrencySecondDeviceSettings) {
	self := startGkmConcurrencySideCase(t, settings.Case, 1)
	defer self.world.Close()
	caseSettings := settings.Case
	ctx, cancel := self.world.StepContext(t.Context(), caseSettings.PhaseTimeout)
	defer cancel()
	memberId := self.network.MemberId()
	repoArgs := []struct {
		name string
		args *sidecar.EvsCreateRepoArgs
	}{
		{name: "general", args: &sidecar.EvsCreateRepoArgs{Kind: protocol.RepoKind_REPO_KIND_GENERAL, Confirmed: true}},
		{name: "group-inbox", args: &sidecar.EvsCreateRepoArgs{Kind: protocol.RepoKind_REPO_KIND_GENERAL, Inbox: protocol.RepoInbox_REPO_INBOX_GROUP, Confirmed: true}},
		// owner-bound to the network's one member, whose consented devices read it
		{name: "vault", args: &sidecar.EvsCreateRepoArgs{Kind: protocol.RepoKind_REPO_KIND_VAULT, OwnerMemberId: &memberId, Confirmed: true}},
	}
	for _, repoArg := range repoArgs {
		if _, err := self.createOtherRepo(ctx, repoArg.name, repoArg.args); err != nil {
			self.world.FailProgress(t, "the repos", err, nil)
		}
	}
	paths := []string{}
	for index := range settings.FileCount {
		paths = append(paths, fmt.Sprintf("quiet-%d.txt", index))
	}
	for _, repo := range self.others {
		for _, path := range paths {
			if err := self.write(ctx, self.owner, repo, path); err != nil {
				self.world.FailProgress(t, "the first device's writes", err, nil)
			}
		}
	}
	if err := self.awaitConvergence(ctx, "the first device's repos", caseSettings.convergenceBound(false)); err != nil {
		self.world.FailProgress(t, "the first device's repos", err, nil)
	}
	before := map[*gkmConcurrencyRepo]*gkmConcurrencyGeneration{}
	for _, repo := range self.repoList() {
		before[repo] = self.newestGeneration(repo)
	}
	self.accountRefusals("the first device")

	// the second device, admitted by the first
	admitted := time.Now()
	added, err := self.network.AddSidecars(ctx, 1, caseSettings.sidecarSettings)
	if err != nil {
		self.world.FailProgress(t, "the second device's enrollment", err, nil)
	}
	if err := self.network.Admit(ctx, added); err != nil {
		self.world.FailProgress(t, "the second device's admission", err, nil)
	}
	second, err := self.addDevice(added[0], true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s stood %s after its enrollment began", second.name, time.Since(admitted).Round(time.Millisecond))
	bound := caseSettings.convergenceBound(false)
	for _, repo := range self.others {
		for _, path := range paths {
			deadline := time.Now().Add(bound)
			for {
				err := worldKeysRead(ctx, second.device, repo.repoId, path, gkmConcurrencyContent(repo, path))
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Errorf("%s reads nothing of %s's %s within %s of its standing: %v", second.name, repo.name, path, bound, err)
					break
				}
				select {
				case <-ctx.Done():
				case <-time.After(caseSettings.PollInterval):
				}
			}
		}
	}
	if err := self.awaitConvergence(ctx, "the second device", bound); err != nil {
		t.Errorf("%v", err)
	}
	// an Add keeps the generation: nothing rotated
	for _, repo := range self.repoList() {
		if newest := self.newestGeneration(repo); before[repo] == nil || newest == nil || newest.rotate.GetGeneration() != before[repo].rotate.GetGeneration() {
			t.Errorf("%s moved from %s to %s at the second device's admission, though an Add keeps the generation", repo.name, describeGkmConcurrencyGeneration(before[repo]), describeGkmConcurrencyGeneration(newest))
		}
	}
	// how the second device took each repo's key: a delivery made with no ask of its own, or one its late ask brought
	for _, repo := range self.repoList() {
		groupId, _ := self.repoGroup(repo)
		late, asks := 0, 0
		for _, delivery := range self.origin.deliveryList() {
			if delivery.recipient == second.sidecarId && delivery.groupId == groupId && delivery.status == 201 {
				late += 1
			}
		}
		for _, ask := range self.origin.askList() {
			if ask.sidecarId == second.sidecarId && ask.groupId == groupId {
				asks += 1
			}
		}
		t.Logf("%s took %s's key by %d deliveries gkm took, after %d late asks of its own", second.name, repo.name, late, asks)
	}
	self.accountRefusals("the second device")
	self.checkGenerations()
	self.checkDeliveries()
	self.checkKeychains()
	self.checkAlarms()
	self.logSummary()
}
