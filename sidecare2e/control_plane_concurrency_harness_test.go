//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The control-plane case's parts that are not its phases (control_plane_concurrency_test.go):
//
//   - scopes: what the token exchange grants a device, read back from the token it minted (its scope claim, read
//     unverified: the token is the device's own, and the claim is what the exchange granted), and what §7.8 and §8
//     give the device, computed from its kind alone (config writer, member device, tagged sidecar) and the repos'
//     kinds, never from the state the exchange reads, so the two are independent; and a device's own api, the
//     Sidecar's on its token with its ClientKey's proofs, which a check calls as the device's sidecar does;
//   - refusals: an enrollment's answer as the sidecar's api client reads it (its status and code), a bare sidecar
//     enrolled from an auth key a test made, and auth keys made through api on a person's credential;
//   - the exact-head races: rounds of every racer previewing at one verified head and all saving at once, as the evs
//     case's third phase races them (evs_concurrency_test.go), here across the networks of the case at once;
//   - api's write budget (§11.13, evsservice's reports) against the objects api wrote: every P0 byte api's process
//     reported against the first metadata cluster is an object it holds under §7.9's and §7.10's prefixes, priced as a
//     create, by kind (log for proj/ and projrows/, revocation for rev/), or a raise of a rev/ object's not_before,
//     priced as its object overwritten, §11.13's overwrite row (§7.10's write row: If-None-Match to create, If-Match
//     to raise), and it reported no P1, P2 or credit; read where the objects listed before and after the report agree,
//     since a report is a level at its time, as the dns and certificates case reads it
//     (dns_certs_concurrency_ledgers_test.go). A raise writes an object the store lists once a second time, as a
//     member's rev/ object is when api's denial and its MemberRevoke's effect round their not_befores to different
//     seconds, so the raises come from the rev/ writer's own count of its writes that landed (model's
//     urnetwork_sidecar_rev_writes_total, outcome raised) since the world began; a PUT spent and never landed, a lost
//     race or a refused one, is counted by neither and still fails the check.
//
// Safe for concurrent use.

package sidecare2e

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/sidecart3"
)

// What a token's claims name, read unverified: the token is one api or its exchange answered a device of the case, and
// the claims are what it minted.
type controlPlaneClaims struct {
	Scope        string     `json:"scope"`
	UserId       server.Id  `json:"user_id"`
	ClientId     *server.Id `json:"client_id"`
	MemberId     *server.Id `json:"member_id"`
	StrictExp    bool       `json:"strict_exp"`
	AdminRefused bool       `json:"admin_refused"`
}

// A token's claims, read unverified.
func controlPlaneTokenClaims(token string) (*controlPlaneClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("the token is no jws")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("the token's claims: %w", err)
	}
	claims := &controlPlaneClaims{}
	if err := json.Unmarshal(payload, claims); err != nil {
		return nil, fmt.Errorf("the token's claims: %w", err)
	}
	return claims, nil
}

// The scopes a token carries, from its claims read unverified.
func controlPlaneTokenScopes(token string) ([]string, error) {
	claims, err := controlPlaneTokenClaims(token)
	if err != nil {
		return nil, err
	}
	return strings.Fields(claims.Scope), nil
}

// The Sidecar's own api on device's current generation, DPoP-signed on its token by its ClientKey, as a component's
// Attach is handed it (ItBridgeWorld.generationApi takes it the same way): what a check calls api through as the
// device's sidecar does, its refresh and its window clients among them.
func controlPlaneDeviceApi(ctx context.Context, device *sidecart3.T3Device) (*sidecar.SidecarApi, error) {
	capture := &controlPlaneApiComponent{attached: make(chan *sidecar.SidecarApi, 1)}
	remove, err := device.Sidecar().AddComponent(capture)
	if err != nil {
		return nil, err
	}
	defer remove()
	select {
	case api := <-capture.attached:
		return api, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s's generation attached no component: %w", device.Name, ctx.Err())
	}
}

// A component that takes the generation's api at its Attach and holds nothing else.
type controlPlaneApiComponent struct {
	attached chan *sidecar.SidecarApi
}

func (self *controlPlaneApiComponent) Name() string {
	return "control-plane-api"
}

func (self *controlPlaneApiComponent) Attach(env *sidecar.SidecarEnv) error {
	if env.SidecarApi != nil {
		select {
		case self.attached <- env.SidecarApi:
		default:
		}
	}
	return nil
}

func (self *controlPlaneApiComponent) Close() {}

// A repo scope as §7.8 spells it.
func controlPlaneRepoScope(repoId sidecar.Id, role string) string {
	return "repo:" + repoId.String() + ":" + role
}

// What the exchange grants device at service of candidates: one fresh exchange asking them all, its token's scope
// claim read back. The exchange's refusal, whole, where it grants none (invalid_scope) or refuses the device
// (invalid_grant).
func controlPlaneGrantedScopes(ctx context.Context, world *World, device *sidecart3.T3Device, service string, candidates []string) (map[string]bool, error) {
	resource := world.T3().Api().ServiceOrigin(service)
	running := device.Sidecar()
	running.ForgetServiceToken(resource, candidates)
	token, err := running.ServiceToken(ctx, resource, candidates)
	if err != nil {
		return nil, err
	}
	scopes, err := controlPlaneTokenScopes(token)
	if err != nil {
		return nil, err
	}
	granted := map[string]bool{}
	for _, scope := range scopes {
		granted[scope] = true
	}
	return granted, nil
}

// One device's kind, as the case made it, which §7.8's and §8's scopes follow.
type controlPlaneDeviceKind struct {
	// a config writer: a device of an AdminSet member, untagged and not read_only (§7.9's Repo row)
	configWriter bool
	// a member's device, untagged (§7.9's commit row: gkm rw on the config)
	member bool
}

// The scopes §7.8 and §8 give a device of kind at service, of the config repo's and, in a team, the team directory's
// r, rw and admin (acl_exchange.go's table, the design's §7.8 bullet): the config's r to every admitted sidecar
// (owner decision 6), its evs rw and admin to config writers alone, its gkm rw to every member device and its gkm admin
// to config writers (QD-894); the directory's r and rw to every admitted sidecar that is not read_only, and its admin
// to its creator and the config writers (§16.11). directoryRepoId zero for a network with none.
func controlPlaneExpectedScopes(kind controlPlaneDeviceKind, service string, configRepoId sidecar.Id, directoryRepoId sidecar.Id) map[string]bool {
	expected := map[string]bool{controlPlaneRepoScope(configRepoId, "r"): true}
	if kind.configWriter {
		expected[controlPlaneRepoScope(configRepoId, "rw")] = true
		expected[controlPlaneRepoScope(configRepoId, "admin")] = true
	}
	if service == WorldBlockGkm && kind.member {
		expected[controlPlaneRepoScope(configRepoId, "rw")] = true
	}
	if directoryRepoId != (sidecar.Id{}) {
		expected[controlPlaneRepoScope(directoryRepoId, "r")] = true
		expected[controlPlaneRepoScope(directoryRepoId, "rw")] = true
		if kind.configWriter {
			expected[controlPlaneRepoScope(directoryRepoId, "admin")] = true
		}
	}
	return expected
}

// The candidates a scope check asks for: r, rw and admin of the config repo and of the directory, where there is one.
func controlPlaneScopeCandidates(configRepoId sidecar.Id, directoryRepoId sidecar.Id) []string {
	candidates := []string{}
	for _, repoId := range []sidecar.Id{configRepoId, directoryRepoId} {
		if repoId == (sidecar.Id{}) {
			continue
		}
		for _, role := range []string{"r", "rw", "admin"} {
			candidates = append(candidates, controlPlaneRepoScope(repoId, role))
		}
	}
	return candidates
}

// The network's config repo and team directory, as device's verified state names them: the Genesis's config repo, and
// the repo of kind directory its rows hold, zero where they hold none.
func controlPlaneNetworkRepos(device *sidecart3.T3Device) (configRepoId sidecar.Id, directoryRepoId sidecar.Id, err error) {
	follower := device.Follower()
	if follower == nil {
		return sidecar.Id{}, sidecar.Id{}, fmt.Errorf("%s follows no log", device.Name)
	}
	state := follower.VerifiedState()
	if state == nil {
		return sidecar.Id{}, sidecar.Id{}, fmt.Errorf("%s holds no verified state", device.Name)
	}
	configRepoId, err = sidecar.IdFromBytes(state.Genesis().GetConfigRepoId())
	if err != nil {
		return sidecar.Id{}, sidecar.Id{}, fmt.Errorf("%s's genesis names no config repo: %w", device.Name, err)
	}
	acl, err := state.Acl()
	if err != nil {
		return sidecar.Id{}, sidecar.Id{}, fmt.Errorf("%s's ACL state: %w", device.Name, err)
	}
	for _, repoId := range acl.RepoIds() {
		if acl.Repo(repoId).GetKind() == protocol.RepoKind_REPO_KIND_DIRECTORY {
			directoryRepoId = repoId
		}
	}
	return configRepoId, directoryRepoId, nil
}

// The status and code of an api refusal as the sidecar's api client read it, 0 and "" for an error that is no api
// refusal.
func controlPlaneApiRefusal(err error) (int, string) {
	var apiErr *sidecar.SidecarApiError
	if errors.As(err, &apiErr) {
		return apiErr.Status, apiErr.Code
	}
	return 0, ""
}

// The limit key of a sidecar limit's refusal (§20.7's QuotaRefusal), "" for another error.
func controlPlaneQuotaRefusalKey(err error) string {
	var apiErr *sidecar.SidecarApiError
	if errors.As(err, &apiErr) && apiErr.QuotaRefusal != nil {
		return apiErr.QuotaRefusal.LimitKey
	}
	return ""
}

// An auth key made through POST /network/auth-keys on root, a person's network credential: max uses, tags (tag ids,
// 32 lowercase hex each) and whether it is ephemeral.
func controlPlaneAuthKey(ctx context.Context, world *World, root string, maxUses int, tags []string, ephemeral bool) (string, error) {
	status, answer, err := world.T3().Api().Call(ctx, http.MethodPost, "/network/auth-keys", root, &model.CreateAuthKeyArgs{
		DurationSeconds: int64(world.T3().Settings().AuthKeyDuration / time.Second),
		MaxUses:         maxUses,
		Tags:            tags,
		Ephemeral:       ephemeral,
	})
	if err != nil {
		return "", err
	}
	result := &model.CreateAuthKeyResult{}
	if status != http.StatusOK || json.Unmarshal(answer, result) != nil || result.AuthKey == "" {
		message := ""
		if result.Error != nil {
			message = result.Error.Message
		}
		return "", &controlPlaneRouteError{route: "POST /network/auth-keys", status: status, message: message}
	}
	return result.AuthKey, nil
}

// A route's refusal as a test's own call through api read it.
type controlPlaneRouteError struct {
	route   string
	status  int
	message string
}

func (self *controlPlaneRouteError) Error() string {
	return fmt.Sprintf("%s: %d %s", self.route, self.status, self.message)
}

// A bare sidecar named name enrolled from authKey, as a device a person set up with that key: started on the world's
// api and exchange, Unenrolled, then Enroll's answer. The sidecar is closed before this returns.
func controlPlaneEnroll(ctx context.Context, world *World, name string, authKey string) error {
	running, err := sidecart3.StartT3Sidecar(world.T3().Context(), world.T3().Net(), name, world.T3().Api(), world.T3().Exchange(), nil)
	if err != nil {
		return err
	}
	defer running.Close()
	if err := running.AwaitState(ctx, sidecar.SidecarStateUnenrolled); err != nil {
		return err
	}
	return running.Sidecar().Enroll(ctx, &sidecar.SidecarEnrollCredential{AuthKey: authKey})
}

// The tag id of tag in device's saved ACL head, as the network's document drew it (§7.9's Writes).
func controlPlaneTagId(ctx context.Context, device *sidecart3.T3Device, source string, tag string) (sidecar.Id, error) {
	plan, err := device.Sidecar().PlanAclPolicy(ctx, []byte(source))
	if err != nil {
		return sidecar.Id{}, err
	}
	table := &protocol.ConfigAtomTable{}
	if tags := plan.BaseHead.File(sidecar.ConfigHeadTagsPath); len(tags) != 0 {
		if err := proto.Unmarshal(tags, table); err != nil {
			return sidecar.Id{}, err
		}
	}
	for _, atom := range table.GetAtoms() {
		if atom.GetName() == tag {
			return sidecar.IdFromBytes(atom.GetAtomId())
		}
	}
	return sidecar.Id{}, fmt.Errorf("%s's head names no tag %s", device.Name, tag)
}

// A tag id as an auth key names it.
func controlPlaneTagHex(tagId sidecar.Id) string {
	return hex.EncodeToString(tagId.Bytes())
}

// One round of a network's exact-head races: the config version every racer previewed at, who won, and why the rest
// were refused.
type controlPlaneRaceRound struct {
	network       string
	configVersion uint64
	wonVersion    uint64
	winners       []string
	refusals      map[string]error
	winnerPin     *bridge.AuthPin
}

// The exact-head races on one network (the case's second phase): each racer saves its own document, every remaining
// racer previewing at the one verified head and all saving at once, each save signing EXACT_HEAD on the head it
// previewed (§7.9's Native ACL confirmation); rounds run until every racer's document landed once. Each round's
// winner's base is synced before the next. The rounds, and an error when a round lands other than one save or runs
// past the racers' count; a refusal other than the head having moved is kept in its round for the caller to judge.
func controlPlaneRace(ctx context.Context, world *World, network *sidecart3.T3Network, racers []*sidecart3.T3Device, document func(index int) string, retry *controlPlaneRetrySettings) ([]*controlPlaneRaceRound, error) {
	remaining := map[*sidecart3.T3Device]int{}
	for index, racer := range racers {
		remaining[racer] = index
	}
	rounds := []*controlPlaneRaceRound{}
	for round := 0; 0 < len(remaining); round += 1 {
		if len(racers) < round {
			return rounds, fmt.Errorf("%s: %d rounds left %d racers' documents unlanded", network.Name, round, len(remaining))
		}
		configVersion, err := controlPlaneConfigVersion(racers[0])
		if err != nil {
			return rounds, err
		}
		record := &controlPlaneRaceRound{network: network.Name, configVersion: configVersion, refusals: map[string]error{}}
		type racerPlan struct {
			racer  *sidecart3.T3Device
			source []byte
			plan   *sidecar.AclSavePlan
			err    error
		}
		plans := []*racerPlan{}
		for racer, index := range remaining {
			plans = append(plans, &racerPlan{racer: racer, source: []byte(document(index))})
		}
		sort.Slice(plans, func(i int, j int) bool { return plans[i].racer.Name < plans[j].racer.Name })
		var group sync.WaitGroup
		for _, plan := range plans {
			group.Go(func() {
				plan.plan, plan.err = controlPlanePlanAcl(ctx, plan.racer, plan.source, retry)
			})
		}
		group.Wait()
		for _, plan := range plans {
			if plan.err != nil {
				return rounds, fmt.Errorf("%s round %d: %s's preview: %w", network.Name, round, plan.racer.Name, plan.err)
			}
		}
		type racerSave struct {
			pin *bridge.AuthPin
			err error
		}
		saves := make([]racerSave, len(plans))
		for index, plan := range plans {
			group.Go(func() {
				pin, err := plan.racer.Sidecar().SaveAclPolicy(ctx, plan.source, plan.plan.Digest, sidecar.SidecarActConfirmed)
				saves[index] = racerSave{pin: pin, err: err}
			})
		}
		group.Wait()
		for index, plan := range plans {
			save := saves[index]
			if save.err == nil {
				record.winners = append(record.winners, plan.racer.Name)
				record.winnerPin = save.pin
				delete(remaining, plan.racer)
				continue
			}
			record.refusals[plan.racer.Name] = save.err
		}
		rounds = append(rounds, record)
		if len(record.winners) != 1 {
			return rounds, fmt.Errorf("%s round %d at config version %d landed %d saves, not one: winners %v, refusals %v", network.Name, round, configVersion, len(record.winners), record.winners, record.refusals)
		}
		if err := network.Sync(ctx, record.winnerPin.GetVersion()); err != nil {
			return rounds, fmt.Errorf("%s round %d: the winner's base: %w", network.Name, round, err)
		}
		if record.wonVersion, err = controlPlaneConfigVersion(racers[0]); err != nil {
			return rounds, err
		}
	}
	return rounds, nil
}

// Whether err refuses a save as the head having moved since its preview, as the design says a confirmed save is
// refused at a changed head (§7.9's Native ACL confirmation: a new preview and confirmation).
func controlPlaneHeadMoved(err error) bool {
	return errors.Is(err, sidecar.ErrConfigHeadConflict) || errors.Is(err, sidecar.ErrAclSaveStale) || errors.Is(err, sidecar.ErrAclSaveUnconfirmed)
}

// How a step asks again while a service answers it is unavailable for now.
type controlPlaneRetrySettings struct {
	Timeout  time.Duration
	Interval time.Duration
}

// device's preview of source, asked again while evs answers unavailable, within retry's bound.
func controlPlanePlanAcl(ctx context.Context, device *sidecart3.T3Device, source []byte, retry *controlPlaneRetrySettings) (*sidecar.AclSavePlan, error) {
	deadline := time.Now().Add(retry.Timeout)
	for {
		plan, err := device.Sidecar().PlanAclPolicy(ctx, source)
		switch {
		case err == nil && plan.PreviewOnly:
			return nil, fmt.Errorf("%s's preview is a preview only: it writes no config head", device.Name)
		case err == nil:
			if err := plan.Tests.Err(); err != nil {
				return nil, fmt.Errorf("the document fails its tests: %w", err)
			}
			return plan, nil
		case evsConcurrencyUnavailable(err) && time.Now().Before(deadline):
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retry.Interval):
			}
		default:
			return nil, err
		}
	}
}

// The config version the newest base device's follower verified names.
func controlPlaneConfigVersion(device *sidecart3.T3Device) (uint64, error) {
	follower := device.Follower()
	if follower == nil || follower.Client() == nil {
		return 0, fmt.Errorf("%s follows no log", device.Name)
	}
	state := follower.Client().State()
	if state == nil || state.Base() == nil {
		return 0, fmt.Errorf("%s holds no base", device.Name)
	}
	return state.Base().GetConfigVersion(), nil
}

// Whether every device's follower holds the same pin as the first's, and each the same newest base's config version:
// the devices that differ, by name, with what each holds.
func controlPlaneDivergent(devices []*sidecart3.T3Device) []string {
	type held struct {
		version       uint64
		chain         string
		configVersion uint64
	}
	read := func(device *sidecart3.T3Device) (held, bool) {
		follower := device.Follower()
		if follower == nil || follower.Client() == nil {
			return held{}, false
		}
		state := follower.Client().State()
		if state == nil || state.Base() == nil {
			return held{}, false
		}
		pin := follower.Client().Pin()
		return held{version: pin.GetVersion(), chain: hex.EncodeToString(pin.GetChain()), configVersion: state.Base().GetConfigVersion()}, true
	}
	divergent := []string{}
	if len(devices) == 0 {
		return divergent
	}
	first, ok := read(devices[0])
	if !ok {
		return []string{devices[0].Name + " follows no log"}
	}
	for _, device := range devices[1:] {
		got, ok := read(device)
		if !ok {
			divergent = append(divergent, device.Name+" follows no log")
			continue
		}
		if got != first {
			divergent = append(divergent, fmt.Sprintf("%s holds n %d, config version %d where %s holds n %d, config version %d", device.Name, got.version, got.configVersion, devices[0].Name, first.version, first.configVersion))
		}
	}
	slices.Sort(divergent)
	return divergent
}

// The token exchange's own judgment of a device's request (oauth.ProjectionExchangeScopeSource, the source api's
// exchange installs), over a gate cache of the case's that every world sync waits on, so it judges at the state a
// sync left, as api's does once its own gate holds it: what "the exchange's projection" of a change is, read apart
// from the device's own view, since a device that learned of its revocation asks for nothing.
type controlPlaneExchange struct {
	source *oauth.ProjectionExchangeScopeSource
	retry  *controlPlaneRetrySettings
}

// The exchange's judgment over a new gate cache of world's, which ends with the world.
func newControlPlaneExchange(world *World, retry *controlPlaneRetrySettings) (*controlPlaneExchange, error) {
	cache, _, err := world.T3().AddGate("control-plane exchange")
	if err != nil {
		return nil, err
	}
	return &controlPlaneExchange{
		source: oauth.NewProjectionExchangeScopeSource(cache, &oauth.ExchangeScopeSettings{Clock: world.T3().Host().Clock}),
		retry:  retry,
	}, nil
}

// What the exchange grants device's client at service of scopes, asked again while the gate has not loaded the
// network (ErrExchangeUnavailable), within the retry's bound.
func (self *controlPlaneExchange) grant(ctx context.Context, device *sidecart3.T3Device, service string, scopes []string) (*oauth.ExchangeScopeGrant, error) {
	identity := device.Sidecar().Identity()
	sidecarId := server.Id(identity.SidecarId)
	request := &oauth.ExchangeScopeRequest{
		NetworkId:       server.Id(identity.NetworkId),
		ClientId:        server.Id(identity.ClientId),
		SubjectClientId: server.Id(identity.ClientId),
		SidecarId:       &sidecarId,
		Audience:        service,
		Scopes:          scopes,
	}
	if device.MemberId != (sidecar.Id{}) {
		memberId := server.Id(device.MemberId)
		request.MemberId = &memberId
	}
	deadline := time.Now().Add(self.retry.Timeout)
	for {
		grant, err := self.source.Grant(ctx, request)
		if err == nil || !errors.Is(err, oauth.ErrExchangeUnavailable) || time.Now().After(deadline) {
			return grant, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(self.retry.Interval):
		}
	}
}

// How the ledgers at a case's end are read (QD-702).
type controlPlaneLedgerSettings struct {
	// Objects one LIST page of the first metadata cluster asks for.
	ObjectPageCount int
	// How many times the objects are listed around a ledger read before the check gives up on their standing still:
	// a read whose objects moved between the two lists is read again.
	BudgetReadTryCount int
}

func defaultControlPlaneLedgerSettings() *controlPlaneLedgerSettings {
	return &controlPlaneLedgerSettings{
		ObjectPageCount:    1000,
		BudgetReadTryCount: 5,
	}
}

// The key prefixes of the objects api's process writes on the first metadata cluster, and the P0 kind it spends each
// one's PUT as: §7.9's log entries and a base's rows (model.ConfigLogSequencer), and §7.10's revocation records
// (model.SidecarRevStore), the member removals' among them.
func controlPlaneApiObjectKinds() map[string]sidecar.WriteP0Kind {
	return map[string]sidecar.WriteP0Kind{
		"proj/":     sidecar.WriteP0KindLog,
		"projrows/": sidecar.WriteP0KindLog,
		"rev/":      sidecar.WriteP0KindRevocation,
	}
}

// Every object under controlPlaneApiObjectKinds' prefixes on the world's first metadata cluster, its byte count by
// key, read a page of pageCount at a time.
func controlPlaneApiObjects(ctx context.Context, world *World, pageCount int) (map[string]int64, error) {
	cluster, err := world.T3().Host().Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	objects := map[string]int64{}
	for prefix := range controlPlaneApiObjectKinds() {
		startAfter := ""
		for {
			page, more, err := cluster.List(ctx, prefix, startAfter, pageCount)
			if err != nil {
				return nil, fmt.Errorf("the objects under %s: %w", prefix, err)
			}
			for _, object := range page {
				objects[object.Key] = object.ByteCount
				startAfter = object.Key
			}
			if !more || len(page) == 0 {
				break
			}
		}
	}
	return objects, nil
}

// The rev/ writer's writes in this process so far that landed, by outcome (model's urnetwork_sidecar_rev_writes_total),
// from the default registry: created, an If-None-Match write of a new object, and raised, an If-Match write of one
// already standing. One world runs in a process at a time, so what a world's checks count is the rise since it began.
type controlPlaneRevWrites struct {
	createdCount float64
	raisedCount  float64
}

// The rev/ writer's writes so far.
func controlPlaneRevWriteCounts() (*controlPlaneRevWrites, error) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return nil, err
	}
	writes := &controlPlaneRevWrites{}
	for _, family := range families {
		if family.GetName() != "urnetwork_sidecar_rev_writes_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() != "outcome" {
					continue
				}
				switch label.GetValue() {
				case "created":
					writes.createdCount = metric.GetCounter().GetValue()
				case "raised":
					writes.raisedCount = metric.GetCounter().GetValue()
				}
			}
		}
	}
	return writes, nil
}

// api's write budget against the objects it wrote, exactly (the file's header): the P0 bytes api's process reported by
// kind equal what the objects it holds price at as created, and each raise of a rev/ object since revWritesAtStart,
// the rev/ writer's writes as the world began (controlPlaneRevWriteCounts), at its object's price overwritten; the
// writer created exactly the rev/ objects the store holds, which holds its count to the store; and it reported nothing
// else.
func controlPlaneCheckApiBudget(t testing.TB, ctx context.Context, world *World, settings *controlPlaneLedgerSettings, revWritesAtStart *controlPlaneRevWrites) {
	t.Helper()
	cluster, err := world.T3().Host().Blobs.FirstCluster()
	if err != nil {
		t.Errorf("the first metadata cluster: %v", err)
		return
	}
	prices := evsservice.DefaultPriceTable4KiB()
	kinds := controlPlaneApiObjectKinds()
	disagreements := []string{}
	for range settings.BudgetReadTryCount {
		writesBefore, err := controlPlaneRevWriteCounts()
		if err != nil {
			t.Errorf("the rev/ writer's writes: %v", err)
			return
		}
		before, err := controlPlaneApiObjects(ctx, world, settings.ObjectPageCount)
		if err != nil {
			t.Errorf("api's objects: %v", err)
			return
		}
		ledgers, err := world.ReadLedgers(ctx, time.Now())
		if err != nil {
			t.Errorf("the ledgers: %v", err)
			return
		}
		after, err := controlPlaneApiObjects(ctx, world, settings.ObjectPageCount)
		if err != nil {
			t.Errorf("api's objects: %v", err)
			return
		}
		writesAfter, err := controlPlaneRevWriteCounts()
		if err != nil {
			t.Errorf("the rev/ writer's writes: %v", err)
			return
		}
		if !maps.Equal(before, after) || *writesBefore != *writesAfter {
			disagreements = append(disagreements, fmt.Sprintf("%d objects, then %d; rev/ writes %+v, then %+v", len(before), len(after), *writesBefore, *writesAfter))
			continue
		}
		want := map[sidecar.WriteP0Kind]int64{}
		objectCounts := map[sidecar.WriteP0Kind]int{}
		revRaisePrices := map[int64]bool{}
		for key, byteCount := range after {
			for prefix, kind := range kinds {
				if !strings.HasPrefix(key, prefix) {
					continue
				}
				price, err := prices.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationCreate, ObjectByteCount: byteCount, Count: 1})
				if err != nil {
					t.Errorf("the price of %s's %d bytes: %v", key, byteCount, err)
					return
				}
				want[kind] += price
				objectCounts[kind] += 1
				if kind == sidecar.WriteP0KindRevocation {
					// a raise is an If-Match PUT over the object: §11.13's overwrite row
					raisePrice, err := prices.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationOverwrite, ObjectByteCount: byteCount, Count: 1})
					if err != nil {
						t.Errorf("the raise price of %s's %d bytes: %v", key, byteCount, err)
						return
					}
					revRaisePrices[raisePrice] = true
				}
			}
		}
		// the writer created each rev/ object the store holds, once
		createCount := int64(writesAfter.createdCount - revWritesAtStart.createdCount)
		if createCount != int64(objectCounts[sidecar.WriteP0KindRevocation]) {
			t.Errorf("the rev/ writer created %d objects since the world began, and api holds %d under rev/", createCount, objectCounts[sidecar.WriteP0KindRevocation])
		}
		// each raise wrote an object again, the object as it stands, at §11.13's overwrite row; every rev/ object
		// prices alike there, its body a few hundred bytes, or a raise's price is not known
		raiseCount := int64(writesAfter.raisedCount - revWritesAtStart.raisedCount)
		if 0 < raiseCount {
			if len(revRaisePrices) != 1 {
				t.Errorf("api raised rev/ objects %d times, and its rev/ objects price at %v overwritten, not one price a raise takes", raiseCount, slices.Sorted(maps.Keys(revRaisePrices)))
				return
			}
			for price := range revRaisePrices {
				want[sidecar.WriteP0KindRevocation] += raiseCount * price
			}
		}
		var report *evsservice.WriteBudgetReport
		for _, candidate := range ledgers.BudgetReports[cluster.ClusterId()] {
			if candidate.ProcessId == ledgers.ApiProcessId {
				report = candidate
			}
		}
		if ledgers.ApiProcessId == "" || report == nil {
			t.Errorf("api's process %q holds no budget report of %s", ledgers.ApiProcessId, cluster.ClusterId())
			return
		}
		got := map[sidecar.WriteP0Kind]int64{}
		for kind, byteCount := range report.P0ByteCounts {
			if byteCount != 0 {
				got[kind] = byteCount
			}
		}
		if !maps.Equal(got, want) {
			t.Errorf("api's process reported P0 %v against %s, and the objects it holds there price at %v as created with %d rev/ raises (%v objects by kind): a write spent and never made, or made and never spent", got, cluster.ClusterId(), want, raiseCount, objectCounts)
		}
		if report.P1ByteCount != 0 || report.UnfundedByteCount != 0 || report.FundedByteCount != 0 || report.CreditByteCount != 0 {
			t.Errorf("api's process reported P1 %d, P2 %d unfunded and %d funded, and credit %d, where all it writes is P0", report.P1ByteCount, report.UnfundedByteCount, report.FundedByteCount, report.CreditByteCount)
		}
		t.Logf("api's write budget: P0 %v against %s for %v objects and %d rev/ raises", got, cluster.ClusterId(), objectCounts, raiseCount)
		return
	}
	t.Errorf("api's objects never stood still across a budget report: %v", disagreements)
}
