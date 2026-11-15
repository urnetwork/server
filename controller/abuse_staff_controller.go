package controller

// §21.10's abuse role and what it does (ABUSE-M5-12; D-ABUSE-5, D-ABUSE-7, R-ABUSE-6, I-6, I-13): the one staff
// route, POST /abuse/action, carries a `kind`, and AbuseStaffAction runs it for an authenticated staff member, as
// plan-ABUSE-staff-surface's decided answer has it (QD-126):
//
//	kind                roles             what it does
//	lookup_case         reviewer, legal   a case by id, with its actions and appeals (model.AbuseReader)
//	lookup_join         reviewer, legal   the cases one join key names (model.AbuseReader)
//	action              reviewer          a person's action under a case, upheld as it is taken (ApplyAbuseAction)
//	lift                reviewer          lifts an action through its case (LiftAbuseAction)
//	review              reviewer          upholds or lifts a machine's action, or reviews a case with none
//	decide_appeal       reviewer          decides an appeal as its second reviewer, a grant lifting as wrong
//	clear_restore_flag  reviewer          clears one network's §12.6 restore flags in an incident
//
// The table grows by RegisterAbuseStaffKind, each later task registering its kinds from its own file: holds
// (ABUSE-M5-14), legal requests and productions (ABUSE-M5-17), limit rows (ABUSE-M5-19), notices (ABUSE-M5-10).
//
// The credential is a per-person bearer, 256 random bits by the runbook's making, which api holds only as a SHA-256
// in vault's abuse-staff.yml (AbuseStaffFileName) with its reviewer id, roles and expiry, compared in constant time
// with every entry; a credential past its expiry, or whose expiry lies further out than CredentialMaxLifetime, 90
// days, is refused, so each is replaced at least that often. A file that is missing or does not parse refuses every
// caller. The lb refuses the route from outside the operators' addresses (warp's rule, not here). "Separate from
// ops" stays a process rule in v0, stated in §21.10: nothing here can enforce it against someone who holds vault or
// Postgres.
//
// Every read goes through model.AbuseReader, which logs it first; a write records its reviewer on its own row
// (plan-ABUSE-metadata-rows #6). TestEveryAbuseReadLogs holds this file and every abuse route to that. Nothing here
// logs a network id, a target, a join key or a reason (I-13).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A staff member's role (QD-126).
type AbuseStaffRole string

const (
	// reviews actions and appeals, acts and lifts, reads cases
	AbuseStaffRoleReviewer AbuseStaffRole = "reviewer"
	// legal process: requests, holds, a production's legal sign-off; reads cases
	AbuseStaffRoleLegal AbuseStaffRole = "legal"
	// a production's engineering sign-off
	AbuseStaffRoleEngineering AbuseStaffRole = "engineering"
)

// Whether this build knows the role.
func (self AbuseStaffRole) known() bool {
	switch self {
	case AbuseStaffRoleReviewer, AbuseStaffRoleLegal, AbuseStaffRoleEngineering:
		return true
	}
	return false
}

// api's verify file among its secret_files: SHA-256 digests only, never a bearer.
const AbuseStaffFileName = "abuse-staff.yml"

// Tunables of the staff credential and its verbs.
type AbuseStaffSettings struct {
	// The furthest a credential's expiry may lie from now: QD-126's 90 days, so each is replaced at least that often.
	// A credential whose expiry lies further out is refused, as an expired one is. Non-positive refuses every one.
	CredentialMaxLifetime time.Duration
}

func DefaultAbuseStaffSettings() *AbuseStaffSettings {
	return &AbuseStaffSettings{
		CredentialMaxLifetime: 90 * 24 * time.Hour,
	}
}

// One authenticated staff member.
type AbuseStaff struct {
	// the opaque id every read, review and decision records (plan-ABUSE-metadata-rows #6)
	ReviewerId server.Id
	Roles      []AbuseStaffRole
}

// Whether the member holds one of roles.
func (self *AbuseStaff) HoldsAny(roles []AbuseStaffRole) bool {
	for _, role := range roles {
		if slices.Contains(self.Roles, role) {
			return true
		}
	}
	return false
}

// A verify file that cannot be used. The text names the part, never a digest.
var ErrAbuseStaffFileRefused = errors.New("abuse staff file refused")

// One entry of the verify file, as written:
//
//	staff:
//	  - reviewer_id: <uuid>
//	    sha256: <hex of the bearer's SHA-256>
//	    roles: [reviewer, legal, engineering]
//	    expires: 2026-12-31T00:00:00Z
type abuseStaffEntryYaml struct {
	ReviewerId string   `yaml:"reviewer_id"`
	Sha256     string   `yaml:"sha256"`
	Roles      []string `yaml:"roles"`
	Expires    string   `yaml:"expires"`
}

type abuseStaffFileYaml struct {
	Staff []abuseStaffEntryYaml `yaml:"staff"`
}

// One credential the file holds.
type abuseStaffEntry struct {
	reviewerId server.Id
	digest     [sha256.Size]byte
	roles      []AbuseStaffRole
	expireTime time.Time
}

// api's view of abuse-staff.yml. Safe for concurrent use; no I/O.
type AbuseStaffVerifier struct {
	entries []abuseStaffEntry
}

// Reads the verify file, refusing it whole when any entry is malformed: a broken file is caught when it is pushed,
// and refuses every caller meanwhile.
func NewAbuseStaffVerifier(data []byte) (*AbuseStaffVerifier, error) {
	refuse := func(format string, arguments ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrAbuseStaffFileRefused, AbuseStaffFileName, fmt.Sprintf(format, arguments...))
	}
	file := &abuseStaffFileYaml{}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(file); err != nil {
		return nil, refuse("%v", err)
	}
	self := &AbuseStaffVerifier{}
	for index, entryYaml := range file.Staff {
		reviewerId, err := server.ParseId(strings.TrimSpace(entryYaml.ReviewerId))
		if err != nil || reviewerId == (server.Id{}) {
			return nil, refuse("entry %d names no reviewer id", index)
		}
		digest, err := hex.DecodeString(strings.TrimSpace(entryYaml.Sha256))
		if err != nil || len(digest) != sha256.Size {
			return nil, refuse("entry %d holds a digest that is not SHA-256 in hex", index)
		}
		roles := []AbuseStaffRole{}
		for _, role := range entryYaml.Roles {
			if !AbuseStaffRole(role).known() {
				return nil, refuse("entry %d names a role this build does not know", index)
			}
			roles = append(roles, AbuseStaffRole(role))
		}
		if len(roles) == 0 {
			return nil, refuse("entry %d holds no role", index)
		}
		expireTime, err := time.Parse(time.RFC3339, strings.TrimSpace(entryYaml.Expires))
		if err != nil {
			return nil, refuse("entry %d holds no RFC 3339 expiry", index)
		}
		self.entries = append(self.entries, abuseStaffEntry{
			reviewerId: reviewerId,
			digest:     [sha256.Size]byte(digest),
			roles:      roles,
			expireTime: expireTime.UTC(),
		})
	}
	if len(self.entries) == 0 {
		return nil, refuse("no staff")
	}
	return self, nil
}

// The staff member whose bearer this is, at now: false for no match, an expired credential, or one whose expiry lies
// further out than the settings allow. The bearer's digest is compared with every entry's in constant time, with no
// early exit, so the time taken says nothing of which entry, if any, it matched.
func (self *AbuseStaffVerifier) Authenticate(bearer string, now time.Time, settings *AbuseStaffSettings) (*AbuseStaff, bool) {
	if bearer == "" {
		return nil, false
	}
	digest := sha256.Sum256([]byte(bearer))
	matchIndex := -1
	for index := range self.entries {
		if subtle.ConstantTimeCompare(digest[:], self.entries[index].digest[:]) == 1 {
			matchIndex = index
		}
	}
	if matchIndex < 0 {
		return nil, false
	}
	entry := self.entries[matchIndex]
	if !now.Before(entry.expireTime) || settings.CredentialMaxLifetime < entry.expireTime.Sub(now) {
		return nil, false
	}
	return &AbuseStaff{ReviewerId: entry.reviewerId, Roles: slices.Clone(entry.roles)}, true
}

// api's verify file from its vault. An error names the part, and the route then refuses every caller.
func LoadAbuseStaffVerifier() (*AbuseStaffVerifier, error) {
	resource, err := server.Vault.SimpleResource(AbuseStaffFileName)
	if err != nil {
		return nil, fmt.Errorf("%w: no %s: %v", ErrAbuseStaffFileRefused, AbuseStaffFileName, err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrAbuseStaffFileRefused, AbuseStaffFileName, err)
	}
	return NewAbuseStaffVerifier(data)
}

// A request whose kind the table does not hold.
var ErrAbuseStaffKindUnknown = errors.New("abuse staff kind unknown")

// The staff member holds none of the kind's roles.
var ErrAbuseStaffRoleRefused = errors.New("abuse staff role refused")

// A request body the kind cannot read, or missing what it needs. The message names the part, never a value.
var ErrAbuseStaffRequestInvalid = errors.New("abuse staff request invalid")

// One staff verb.
type AbuseStaffKind struct {
	// the request's kind
	Name string
	// the roles any one of which may run it
	Roles []AbuseStaffRole
	// runs it for staff on the request's whole body, kind included, answering the result's body
	Run func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error)
}

// The verbs by name, set from inits.
var abuseStaffKinds = struct {
	stateLock sync.Mutex
	nameKinds map[string]*AbuseStaffKind
}{
	nameKinds: map[string]*AbuseStaffKind{},
}

// Adds a verb to the table, from a file's init. Two verbs of one name, or one with no role, panic: either would leave
// a request's meaning to registration order.
func RegisterAbuseStaffKind(kind *AbuseStaffKind) {
	abuseStaffKinds.stateLock.Lock()
	defer abuseStaffKinds.stateLock.Unlock()
	if kind.Name == "" || len(kind.Roles) == 0 || kind.Run == nil {
		panic(fmt.Sprintf("abuse staff kind %q has no name, role or run", kind.Name))
	}
	if _, taken := abuseStaffKinds.nameKinds[kind.Name]; taken {
		panic(fmt.Sprintf("two abuse staff kinds are registered as %q", kind.Name))
	}
	abuseStaffKinds.nameKinds[kind.Name] = kind
}

// The verb of a name, or nil.
func abuseStaffKindOf(name string) *AbuseStaffKind {
	abuseStaffKinds.stateLock.Lock()
	defer abuseStaffKinds.stateLock.Unlock()
	return abuseStaffKinds.nameKinds[name]
}

// The names the table holds, sorted.
func AbuseStaffKindNames() []string {
	abuseStaffKinds.stateLock.Lock()
	defer abuseStaffKinds.stateLock.Unlock()
	names := []string{}
	for name := range abuseStaffKinds.nameKinds {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// POST /abuse/action's answer: the kind run and its result.
type AbuseStaffResult struct {
	Kind   string `json:"kind"`
	Result any    `json:"result"`
}

// Runs one staff request for staff: reads its kind, checks the member holds one of the kind's roles, and runs it.
func AbuseStaffAction(ctx context.Context, staff *AbuseStaff, body []byte) (*AbuseStaffResult, error) {
	request := &struct {
		Kind string `json:"kind"`
	}{}
	if err := json.Unmarshal(body, request); err != nil {
		return nil, fmt.Errorf("%w: the body is not a JSON object with a kind", ErrAbuseStaffRequestInvalid)
	}
	kind := abuseStaffKindOf(request.Kind)
	if kind == nil {
		return nil, ErrAbuseStaffKindUnknown
	}
	if !staff.HoldsAny(kind.Roles) {
		return nil, ErrAbuseStaffRoleRefused
	}
	result, err := kind.Run(ctx, staff, body)
	if err != nil {
		return nil, err
	}
	return &AbuseStaffResult{Kind: kind.Name, Result: result}, nil
}

// Decodes a kind's whole body into args, refusing a field the kind does not take.
func decodeAbuseStaffArgs(body []byte, args any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(args); err != nil {
		return fmt.Errorf("%w: the body does not read as the kind's", ErrAbuseStaffRequestInvalid)
	}
	return nil
}

// The verbs' seams, which a test replaces.
var abuseStaffDeps = struct {
	now      func() time.Time
	settings func() *model.AbuseSettings
}{
	now: server.NowUtc,
	settings: func() *model.AbuseSettings {
		settings := model.DefaultAbuseSettings()
		model.ApplyAbuseConfig(settings, server.GetAbuseConfig())
		return settings
	},
}

// Refuses reasons a review row cannot hold.
func validateAbuseStaffReason(reason string, settings *model.AbuseSettings) error {
	switch {
	case strings.TrimSpace(reason) == "":
		return fmt.Errorf("%w: no reason", ErrAbuseStaffRequestInvalid)
	case settings.ReviewReasonMaxByteCount < len(reason):
		return fmt.Errorf("%w: a reason past its bound", ErrAbuseStaffRequestInvalid)
	case !utf8.ValidString(reason) || strings.ContainsRune(reason, 0):
		return fmt.Errorf("%w: a reason that is not text", ErrAbuseStaffRequestInvalid)
	}
	return nil
}

// A case as the abuse role reads it.
type AbuseStaffCaseView struct {
	CaseId          server.Id  `json:"case_id"`
	NetworkId       *server.Id `json:"network_id,omitempty"`
	Category        string     `json:"category"`
	TargetKind      string     `json:"target_kind"`
	Target          string     `json:"target"`
	ReporterClass   string     `json:"reporter_class"`
	JoinKind        string     `json:"join_kind"`
	JoinKey         string     `json:"join_key"`
	State           string     `json:"state"`
	Barred          bool       `json:"barred"`
	ReviewDueTimeMs int64      `json:"review_due_time_ms"`
	CreateTimeMs    int64      `json:"create_time_ms"`
	CloseTimeMs     int64      `json:"close_time_ms"`
}

// An action or lift as the abuse role reads it.
type AbuseStaffActionView struct {
	ActionId     server.Id  `json:"action_id"`
	CaseId       server.Id  `json:"case_id"`
	Kind         string     `json:"kind"`
	Target       string     `json:"target"`
	UntilTimeMs  int64      `json:"until_time_ms"`
	ByMachine    bool       `json:"by_machine"`
	ReviewState  string     `json:"review_state"`
	ReviewerId   *server.Id `json:"reviewer_id,omitempty"`
	ReviewReason string     `json:"review_reason"`
	ReviewTimeMs int64      `json:"review_time_ms"`
	AppliedTime  int64      `json:"applied_time_ms"`
	MailedTime   int64      `json:"mailed_time_ms"`
	CreateTimeMs int64      `json:"create_time_ms"`
}

// An appeal as the abuse role reads it: the appellant's reason, never the appellant.
type AbuseStaffAppealView struct {
	AppealId       server.Id  `json:"appeal_id"`
	CaseId         server.Id  `json:"case_id"`
	State          string     `json:"state"`
	Reason         string     `json:"reason"`
	ReviewerId     *server.Id `json:"reviewer_id,omitempty"`
	DecisionReason string     `json:"decision_reason"`
	CreateTimeMs   int64      `json:"create_time_ms"`
	DecideTimeMs   int64      `json:"decide_time_ms"`
}

// Unix milliseconds, 0 for none.
func abuseStaffTimeMs(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMilli()
}

func abuseStaffCaseView(abuseCase *model.AbuseCase) *AbuseStaffCaseView {
	if abuseCase == nil {
		return nil
	}
	return &AbuseStaffCaseView{
		CaseId:          abuseCase.CaseId,
		NetworkId:       abuseCase.NetworkId,
		Category:        string(abuseCase.Category),
		TargetKind:      string(abuseCase.TargetKind),
		Target:          abuseCase.Target,
		ReporterClass:   string(abuseCase.ReporterClass),
		JoinKind:        string(abuseCase.JoinKind),
		JoinKey:         abuseCase.JoinKey,
		State:           string(abuseCase.State),
		Barred:          abuseCase.Barred,
		ReviewDueTimeMs: abuseStaffTimeMs(abuseCase.ReviewDueTime),
		CreateTimeMs:    abuseCase.CreateTime.UnixMilli(),
		CloseTimeMs:     abuseStaffTimeMs(abuseCase.CloseTime),
	}
}

func abuseStaffActionView(action *model.AbuseAction) *AbuseStaffActionView {
	if action == nil {
		return nil
	}
	return &AbuseStaffActionView{
		ActionId:     action.ActionId,
		CaseId:       action.CaseId,
		Kind:         string(action.Kind),
		Target:       action.Target,
		UntilTimeMs:  abuseStaffTimeMs(action.UntilTime),
		ByMachine:    action.ByMachine,
		ReviewState:  string(action.ReviewState),
		ReviewerId:   action.ReviewerId,
		ReviewReason: action.ReviewReason,
		ReviewTimeMs: abuseStaffTimeMs(action.ReviewTime),
		AppliedTime:  abuseStaffTimeMs(action.AppliedTime),
		MailedTime:   abuseStaffTimeMs(action.MailedTime),
		CreateTimeMs: action.CreateTime.UnixMilli(),
	}
}

func abuseStaffAppealView(appeal *model.AbuseAppeal) *AbuseStaffAppealView {
	return &AbuseStaffAppealView{
		AppealId:       appeal.AppealId,
		CaseId:         appeal.CaseId,
		State:          string(appeal.State),
		Reason:         appeal.Reason,
		ReviewerId:     appeal.ReviewerId,
		DecisionReason: appeal.DecisionReason,
		CreateTimeMs:   appeal.CreateTime.UnixMilli(),
		DecideTimeMs:   abuseStaffTimeMs(appeal.DecideTime),
	}
}

// What an action or lift did, as the staff route answers it.
type AbuseStaffActionResult struct {
	Action   *AbuseStaffActionView `json:"action"`
	Inserted bool                  `json:"inserted"`
	Applied  bool                  `json:"applied"`
	Mailed   bool                  `json:"mailed"`
	// a lifted revocation stands: the network's devices order new certificates (§21.6)
	RevocationStands bool `json:"revocation_stands"`
}

func abuseStaffActionResultOf(result *AbuseActionResult) *AbuseStaffActionResult {
	return &AbuseStaffActionResult{
		Action:           abuseStaffActionView(result.Action),
		Inserted:         result.Inserted,
		Applied:          result.Applied,
		Mailed:           result.Mailed,
		RevocationStands: result.RevocationStands,
	}
}

// The unit an action kind acts on, for §21.4's never-the-root check.
func abuseStaffActionTargetKind(kind model.AbuseActionKind) model.AbuseTargetKind {
	switch kind {
	case model.AbuseActionKindLabelKill, model.AbuseActionKindZoneSuspend:
		return model.AbuseTargetKindLabel
	case model.AbuseActionKindMailHalt, model.AbuseActionKindMailLabelHalt:
		return model.AbuseTargetKindSendingDomain
	case model.AbuseActionKindNetworkSuspend:
		return model.AbuseTargetKindNetwork
	}
	return model.AbuseTargetKindHost
}

// lookup_case's body.
type abuseStaffLookupCaseArgs struct {
	Kind   string    `json:"kind"`
	CaseId server.Id `json:"case_id"`
}

// lookup_case's answer.
type AbuseStaffCaseFileResult struct {
	Case    *AbuseStaffCaseView     `json:"case"`
	Actions []*AbuseStaffActionView `json:"actions"`
	Appeals []*AbuseStaffAppealView `json:"appeals"`
	More    bool                    `json:"more"`
}

// lookup_join's body.
type abuseStaffLookupJoinArgs struct {
	Kind     string `json:"kind"`
	JoinKind string `json:"join_kind"`
	JoinKey  string `json:"join_key"`
}

// lookup_join's answer.
type AbuseStaffJoinResult struct {
	Cases []*AbuseStaffCaseView `json:"cases"`
	More  bool                  `json:"more"`
}

// action's body.
type abuseStaffActionArgs struct {
	Kind   string    `json:"kind"`
	CaseId server.Id `json:"case_id"`
	// the caller's id for the action, so a retry records it once; absent draws one
	ActionId   *server.Id `json:"action_id"`
	ActionKind string     `json:"action_kind"`
	Target     string     `json:"target"`
	// when the action ends by itself; 0 when only a lift ends it
	UntilTimeMs int64  `json:"until_time_ms"`
	Reason      string `json:"reason"`
}

// lift's body.
type abuseStaffLiftArgs struct {
	Kind   string    `json:"kind"`
	CaseId server.Id `json:"case_id"`
	// the action lifted
	ActionId server.Id `json:"action_id"`
	// the lift's own id, so a retry records it once; absent draws one
	LiftId *server.Id `json:"lift_id"`
	// review found the action wrong, which takes it out of every count
	Wrong  bool   `json:"wrong"`
	Reason string `json:"reason"`
}

// review's decisions.
const (
	abuseStaffReviewUphold    = "uphold"
	abuseStaffReviewLift      = "lift"
	abuseStaffReviewLiftWrong = "lift_wrong"
	abuseStaffReviewNoAction  = "no_action"
)

// review's body.
type abuseStaffReviewArgs struct {
	Kind   string    `json:"kind"`
	CaseId server.Id `json:"case_id"`
	// the action reviewed; absent for no_action
	ActionId *server.Id `json:"action_id"`
	// uphold, lift, lift_wrong, or no_action for a case with none
	Decision string `json:"decision"`
	// a lift's own id; absent draws one
	LiftId *server.Id `json:"lift_id"`
	Reason string     `json:"reason"`
}

// review's answer: the review recorded, or the lift it ran.
type AbuseStaffReviewResult struct {
	Action   *AbuseStaffActionView   `json:"action,omitempty"`
	Case     *AbuseStaffCaseView     `json:"case,omitempty"`
	Recorded bool                    `json:"recorded"`
	Lift     *AbuseStaffActionResult `json:"lift,omitempty"`
}

// decide_appeal's body.
type abuseStaffDecideAppealArgs struct {
	Kind     string    `json:"kind"`
	AppealId server.Id `json:"appeal_id"`
	// granted or denied
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// decide_appeal's answer: the decision and, for a grant, each lift it ran.
type AbuseStaffDecideAppealResult struct {
	Appeal   *AbuseStaffAppealView     `json:"appeal"`
	Case     *AbuseStaffCaseView       `json:"case"`
	Recorded bool                      `json:"recorded"`
	Lifts    []*AbuseStaffActionResult `json:"lifts"`
}

// clear_restore_flag's body.
type abuseStaffClearRestoreFlagArgs struct {
	Kind       string    `json:"kind"`
	Root       string    `json:"root"`
	IncidentId server.Id `json:"incident_id"`
	NetworkId  server.Id `json:"network_id"`
}

// clear_restore_flag's answer.
type AbuseStaffClearRestoreFlagResult struct {
	ClearedCount int `json:"cleared_count"`
}

// Lifts one action of a case for staff as a review: §21's one path, with the reviewer and the reason.
func abuseStaffLift(ctx context.Context, staff *AbuseStaff, caseId server.Id, liftedActionId server.Id, liftId *server.Id, wrong bool, reason string) (*AbuseStaffActionResult, error) {
	actionId := server.NewId()
	if liftId != nil {
		actionId = *liftId
	}
	reviewState := model.AbuseReviewStateLifted
	if wrong {
		reviewState = model.AbuseReviewStateLiftedWrong
	}
	reviewerId := staff.ReviewerId
	result, err := LiftAbuseAction(ctx, &model.AbuseLiftArgs{
		ActionId:       actionId,
		CaseId:         caseId,
		LiftedActionId: liftedActionId,
		ReviewState:    reviewState,
		ReviewerId:     &reviewerId,
		ReviewReason:   reason,
	})
	if err != nil {
		return nil, err
	}
	return abuseStaffActionResultOf(result), nil
}

// The verbs this task builds.
func init() {
	lookupRoles := []AbuseStaffRole{AbuseStaffRoleReviewer, AbuseStaffRoleLegal}
	reviewerRoles := []AbuseStaffRole{AbuseStaffRoleReviewer}

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "lookup_case",
		Roles: lookupRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffLookupCaseArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			reader, err := model.NewAbuseReader(staff.ReviewerId, abuseStaffDeps.settings())
			if err != nil {
				return nil, err
			}
			caseFile, err := reader.LookupAbuseCase(ctx, args.CaseId)
			if err != nil {
				return nil, err
			}
			result := &AbuseStaffCaseFileResult{
				Case:    abuseStaffCaseView(caseFile.Case),
				Actions: []*AbuseStaffActionView{},
				Appeals: []*AbuseStaffAppealView{},
				More:    caseFile.More,
			}
			for _, action := range caseFile.Actions {
				result.Actions = append(result.Actions, abuseStaffActionView(action))
			}
			for _, appeal := range caseFile.Appeals {
				result.Appeals = append(result.Appeals, abuseStaffAppealView(appeal))
			}
			return result, nil
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "lookup_join",
		Roles: lookupRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffLookupJoinArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			reader, err := model.NewAbuseReader(staff.ReviewerId, abuseStaffDeps.settings())
			if err != nil {
				return nil, err
			}
			lookup, err := reader.LookupAbuseCasesByJoinKey(ctx, model.AbuseJoinKind(args.JoinKind), args.JoinKey)
			if err != nil {
				return nil, err
			}
			result := &AbuseStaffJoinResult{Cases: []*AbuseStaffCaseView{}, More: lookup.More}
			for _, abuseCase := range lookup.Cases {
				result.Cases = append(result.Cases, abuseStaffCaseView(abuseCase))
			}
			return result, nil
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "action",
		Roles: reviewerRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffActionArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			settings := abuseStaffDeps.settings()
			if err := validateAbuseStaffReason(args.Reason, settings); err != nil {
				return nil, err
			}
			kind := model.AbuseActionKind(args.ActionKind)
			// never the root or a platform name (§21.4): the ladders' own check, with a category that decides nothing
			// else
			target := model.AbuseTarget{Kind: abuseStaffActionTargetKind(kind), Name: args.Target}
			if _, err := model.NextAbuseStep(model.AbuseCategoryOther, target, nil, abuseStaffDeps.now(), &settings.AbusePolicySettings); err != nil {
				return nil, err
			}
			actionId := server.NewId()
			if args.ActionId != nil {
				actionId = *args.ActionId
			}
			var untilTime *time.Time
			if 0 < args.UntilTimeMs {
				until := time.UnixMilli(args.UntilTimeMs).UTC()
				untilTime = &until
			}
			reviewerId := staff.ReviewerId
			result, err := ApplyAbuseAction(ctx, &model.AbuseActionArgs{
				ActionId:     actionId,
				CaseId:       args.CaseId,
				Kind:         kind,
				Target:       args.Target,
				UntilTime:    untilTime,
				ReviewerId:   &reviewerId,
				ReviewReason: args.Reason,
			})
			if err != nil {
				return nil, err
			}
			return abuseStaffActionResultOf(result), nil
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "lift",
		Roles: reviewerRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffLiftArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			if err := validateAbuseStaffReason(args.Reason, abuseStaffDeps.settings()); err != nil {
				return nil, err
			}
			return abuseStaffLift(ctx, staff, args.CaseId, args.ActionId, args.LiftId, args.Wrong, args.Reason)
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "review",
		Roles: reviewerRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffReviewArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			settings := abuseStaffDeps.settings()
			if err := validateAbuseStaffReason(args.Reason, settings); err != nil {
				return nil, err
			}
			switch args.Decision {
			case abuseStaffReviewUphold, abuseStaffReviewNoAction:
				reviewArgs := &model.AbuseReviewArgs{CaseId: args.CaseId, ReviewerId: staff.ReviewerId, Reason: args.Reason}
				if args.Decision == abuseStaffReviewUphold {
					if args.ActionId == nil {
						return nil, fmt.Errorf("%w: an uphold names no action", ErrAbuseStaffRequestInvalid)
					}
					reviewArgs.ActionId = args.ActionId
				} else if args.ActionId != nil {
					return nil, fmt.Errorf("%w: a case's own review names an action", ErrAbuseStaffRequestInvalid)
				}
				review, err := model.RecordAbuseReview(ctx, reviewArgs, abuseStaffDeps.now(), settings)
				if err != nil {
					return nil, err
				}
				return &AbuseStaffReviewResult{
					Action:   abuseStaffActionView(review.Action),
					Case:     abuseStaffCaseView(review.Case),
					Recorded: review.Recorded,
				}, nil
			case abuseStaffReviewLift, abuseStaffReviewLiftWrong:
				if args.ActionId == nil {
					return nil, fmt.Errorf("%w: a lift names no action", ErrAbuseStaffRequestInvalid)
				}
				lift, err := abuseStaffLift(ctx, staff, args.CaseId, *args.ActionId, args.LiftId, args.Decision == abuseStaffReviewLiftWrong, args.Reason)
				if err != nil {
					return nil, err
				}
				return &AbuseStaffReviewResult{Recorded: lift.Inserted, Lift: lift}, nil
			}
			return nil, fmt.Errorf("%w: an unknown decision", ErrAbuseStaffRequestInvalid)
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "decide_appeal",
		Roles: reviewerRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffDecideAppealArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			settings := abuseStaffDeps.settings()
			if err := validateAbuseStaffReason(args.Reason, settings); err != nil {
				return nil, err
			}
			decision, err := model.DecideAbuseAppeal(ctx, &model.AbuseAppealDecisionArgs{
				AppealId:   args.AppealId,
				ReviewerId: staff.ReviewerId,
				State:      model.AbuseAppealState(args.Decision),
				Reason:     args.Reason,
			}, abuseStaffDeps.now(), settings)
			if err != nil {
				return nil, err
			}
			result := &AbuseStaffDecideAppealResult{
				Appeal:   abuseStaffAppealView(decision.Appeal),
				Case:     abuseStaffCaseView(decision.Case),
				Recorded: decision.Recorded,
				Lifts:    []*AbuseStaffActionResult{},
			}
			for _, actionId := range decision.LiftActionIds {
				lift, err := abuseStaffLift(ctx, staff, decision.Appeal.CaseId, actionId, nil, true, args.Reason)
				if errors.Is(err, model.ErrAbuseActionLifted) {
					// another lift took it between the decision and this one
					continue
				}
				if err != nil {
					// the decision stands: deciding it again the same way lifts what is left
					return nil, err
				}
				result.Lifts = append(result.Lifts, lift)
			}
			return result, nil
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "clear_restore_flag",
		Roles: reviewerRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffClearRestoreFlagArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			if args.Root == "" || args.IncidentId == (server.Id{}) || args.NetworkId == (server.Id{}) {
				return nil, fmt.Errorf("%w: a flag names no root, incident or network", ErrAbuseStaffRequestInvalid)
			}
			clearedCount, err := model.ClearDnsRestoreFlag(ctx, args.Root, args.IncidentId, args.NetworkId, abuseStaffDeps.now())
			if err != nil {
				return nil, err
			}
			return &AbuseStaffClearRestoreFlagResult{ClearedCount: clearedCount}, nil
		},
	})
}
