package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The P0 auth fix, §7.4's stage 1 (D-TEAM-1, I-6): no sidecar or agent
// credential administers the network or mints a login. A client whose tokens
// are in a sidecar's hands is marked in network_client_owner at mint, and a
// child of any client is refused alike. The router asks ClientRefusesAdmin on
// the 27 app admin and 10 own client payout routes (api/route_authz.go).
//
// The owner row belongs to migration group MG0, which is pending until its
// Gn-out. A binary that does not link MG0 reads only network_client, so it still
// sees every child, it marks nothing, and it refuses a request for a mark
// (server.SidecarMg0Linked).

// Settings of the client admin check.
type ClientAdminSettings struct {
	// The clients whose answer a process keeps, 32,768 to match clientAuthTimes'
	// bound. Each answer is immutable, so a full map clears and costs one more
	// read per client, never a wrong answer.
	CachedClientMaxCount int64
}

// The cache's bound.
func DefaultClientAdminSettings() *ClientAdminSettings {
	return &ClientAdminSettings{
		CachedClientMaxCount: 32 * 1024,
	}
}

// What this process has read. Each answer is immutable.
var clientRefusesAdminAnswers = newBoundedClientMap[bool](DefaultClientAdminSettings().CachedClientMaxCount)

var clientAdminLookupErrorCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "auth",
	Name:      "client_admin_lookup_errors_total",
	Help:      "Client admin checks with no answer: a failed read, or a client the token's network does not have. Each refuses, so urnetwork_auth_child_admin_calls_total counts it too",
})

func init() {
	prometheus.MustRegister(clientAdminLookupErrorCounter)
}

// Whether the client's tokens are refused on the admin routes that an ordinary
// network's client tokens still reach. True for a child (source_client_id is
// set), for an admin_refused owner row, and when the answer cannot be read. A
// top-level client with no owner row is a legacy client: the shipped apps call
// these routes with such a token until stage 3.
//
// Neither fact ever changes for a client: source_client_id and the owner row are
// written with the client. So an answer is cached for as long as this process
// can hold it, and only a failed lookup is asked again.
func ClientRefusesAdmin(ctx context.Context, networkId server.Id, clientId server.Id) bool {
	if refuses, ok := clientRefusesAdminAnswers.Load(clientId); ok {
		return refuses
	}
	sql := `
		SELECT network_client.source_client_id IS NOT NULL
		FROM network_client
		WHERE
			network_client.client_id = $1 AND
			network_client.network_id = $2
	`
	if server.SidecarMg0Linked() {
		sql = `
			SELECT
				network_client.source_client_id IS NOT NULL OR
				COALESCE(network_client_owner.admin_refused, false)
			FROM network_client
			LEFT JOIN network_client_owner ON network_client_owner.client_id = network_client.client_id
			WHERE
				network_client.client_id = $1 AND
				network_client.network_id = $2
		`
	}
	found := false
	refuses := false
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, sql, clientId, networkId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&refuses))
					found = true
				}
			})
		})
	}); r != nil || !found {
		clientAdminLookupErrorCounter.Inc()
		if glog.V(1) {
			glog.Infof("[auth][%s]client admin check has no answer for client %s\n", networkId, clientId)
		}
		return true
	}
	clientRefusesAdminAnswers.Store(clientId, refuses)
	return refuses
}

// Drops this process's cached answers, for a test that writes a client's rows
// directly after the client was asked about.
func Testing_ForgetClientRefusesAdmin() {
	clientRefusesAdminAnswers.Clear()
}

// A client's marks (§7.4, §7.7), set at mint and never changed. A marked
// client's children inherit every one.
type clientMarks struct {
	// the client's tokens never administer the network
	adminRefused bool
	// the client's tokens are sidecar-held: 7-day expiry, the route allowlist and
	// the ClientKey proof (D-TEAM-14). Only sidecars send it; an app device minted
	// admin_refused, and so its window clients, carry none (owner decision 131).
	strictExp bool
	// the client provides no egress (§20.8): exempt from the concurrent and
	// create caps and unlisted, carried in its tokens so connect needs no query
	declaresNoEgress bool
	// the device client of an sdk whose forwarding role left legacy, which its
	// resident tells team hints (§18.6, owner decision 106)
	aclEnforcing bool
	// a sidecar's forwarding client F, which its app's VPN component holds (§7.4,
	// owner decision 197): strict_exp and admin_refused too, its tokens reach
	// only forwardOnlyRoutes and exchange only for the config's r (§7.8). MG8's
	// column, so only a binary that links MG8 reads or writes it
	forwardOnly bool
}

// Whether there is anything to write in an owner row.
func (self clientMarks) marked() bool {
	return self.adminRefused || self.strictExp || self.declaresNoEgress || self.aclEnforcing || self.forwardOnly
}

// Both sets of marks: what a request asks for and what a parent passes down.
func (self clientMarks) union(other clientMarks) clientMarks {
	return clientMarks{
		adminRefused:     self.adminRefused || other.adminRefused,
		strictExp:        self.strictExp || other.strictExp,
		declaresNoEgress: self.declaresNoEgress || other.declaresNoEgress,
		aclEnforcing:     self.aclEnforcing || other.aclEnforcing,
		forwardOnly:      self.forwardOnly || other.forwardOnly,
	}
}

// What a request asks for. A strict_exp client is always admin_refused, so a
// request for the one alone gets both, and a forward_only client is both.
func requestedClientMarks(authClient *AuthNetworkClientArgs) clientMarks {
	return clientMarks{
		adminRefused:     authClient.AdminRefused || authClient.StrictExp || authClient.ForwardOnly,
		strictExp:        authClient.StrictExp || authClient.ForwardOnly,
		declaresNoEgress: authClient.DeclaresNoEgress,
		aclEnforcing:     authClient.AclEnforcing,
		forwardOnly:      authClient.ForwardOnly,
	}
}

// The client's marks, none for a client of another network or with no owner row.
func readClientMarksInTx(ctx context.Context, tx server.PgTx, networkId server.Id, clientId server.Id) (marks clientMarks) {
	if !server.SidecarMg0Linked() {
		return
	}
	// forward_only is MG8's: a binary without it reads none
	forwardOnlyColumn := "false"
	if server.SidecarMg8Linked() {
		forwardOnlyColumn = "COALESCE(network_client_owner.forward_only, false)"
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT
				network_client_owner.admin_refused,
				network_client_owner.strict_exp,
				network_client_owner.declares_no_egress,
				network_client_owner.acl_enforcing,
				`+forwardOnlyColumn+`
			FROM network_client_owner
			INNER JOIN network_client ON network_client.client_id = network_client_owner.client_id
			WHERE
				network_client_owner.client_id = $1 AND
				network_client.network_id = $2
		`,
		clientId,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&marks.adminRefused, &marks.strictExp, &marks.declaresNoEgress, &marks.aclEnforcing, &marks.forwardOnly))
		}
	})
	return
}

// A new client's owner row (§7.7), written at its mint and deleted with it.
type clientOwnerInsert struct {
	// the user who owns an untagged client; nil for a tagged one
	ownerUserId *server.Id
	// the member whose device it is: the owner's user id in a personal network
	// (§7.5); nil for a tagged client
	memberId *server.Id
	// a tagged client's admitting member (§7.7), mirrored from SponsorSet later
	sponsorUserId *server.Id
	// the auth key it enrolled with, nil for any other mint
	authKeyId *server.Id
	tagged    bool
	ephemeral bool
	marks     clientMarks
	// enroll's Ed25519 key (plan-TEAM-enroll-client-key), nil for any other mint
	clientPublicKey []byte
}

// Writes a new client's owner row. The caller checked the marks.
// client_public_key is MG1's column and forward_only MG8's, so a row that has
// either needs its group.
func insertClientOwnerInTx(ctx context.Context, tx server.PgTx, clientId server.Id, row *clientOwnerInsert) {
	columns := []string{
		"client_id",
		"owner_user_id",
		"member_id",
		"sponsor_user_id",
		"auth_key_id",
		"tagged",
		"ephemeral",
		"admin_refused",
		"strict_exp",
		"declares_no_egress",
		"acl_enforcing",
	}
	values := []any{
		clientId,
		row.ownerUserId,
		row.memberId,
		row.sponsorUserId,
		row.authKeyId,
		row.tagged,
		row.ephemeral,
		row.marks.adminRefused,
		row.marks.strictExp,
		row.marks.declaresNoEgress,
		row.marks.aclEnforcing,
	}
	if row.clientPublicKey != nil {
		if !server.SidecarMg1Linked() {
			panic(errors.New("an owner row's client_public_key needs MG1"))
		}
		columns = append(columns, "client_public_key")
		values = append(values, row.clientPublicKey)
	}
	if row.marks.forwardOnly {
		if !server.SidecarMg8Linked() {
			panic(errors.New("an owner row's forward_only needs MG8"))
		}
		columns = append(columns, "forward_only")
		values = append(values, true)
	}
	placeholders := make([]string, len(columns))
	for i := range columns {
		placeholders[i] = "$" + strconv.Itoa(i+1)
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`INSERT INTO network_client_owner (`+strings.Join(columns, ", ")+`) VALUES (`+strings.Join(placeholders, ", ")+`)`,
		values...,
	))
}

const (
	clientMarksUnavailableMessage = "This server cannot mark a client yet."
	clientMarksImmutableMessage   = "A client's marks are assigned at creation and cannot be changed."
	// §7.7: an ephemeral client is minted new and top-level, with its lease
	clientEphemeralTopLevelMessage = "Only a new top-level client can be ephemeral."
	// an auth key that does not admit: unknown, expired or spent, said alike
	networkEnrollAuthKeyInvalidMessage = "The auth key is not valid."
	// D-TRUST-3: until stage 2 is live, a strict_exp client exists only in a
	// network sidecar.yml's preview_networks lists (SidecarPreviewAllowed)
	clientStrictExpNotPreviewMessage = SidecarPreviewRefusedMessage
	// a hosted proxy's host mints its token with no mark, so a strict_exp client
	// never has one
	clientStrictExpProxyMessage = "A sidecar client cannot host a proxy."
)

// Why a mint of a client with these marks is refused, or "". Every mint that ends
// in a strict_exp token asks, an inherited mark included.
func refuseClientMarks(networkId server.Id, marks clientMarks, proxy bool) string {
	if !marks.strictExp {
		return ""
	}
	if !SidecarPreviewAllowed(networkId) {
		return clientStrictExpNotPreviewMessage
	}
	if proxy {
		return clientStrictExpProxyMessage
	}
	return ""
}

// The client has no key a proof could be checked against.
var ErrClientProofKeyNotBound = errors.New("Client has no bound key.")

// The key that proves a strict_exp bearer's ClientKey (§7.4): the client's newest
// sidecar_identity_binding key, else the client_public_key POST /network/enroll
// stored in its owner row (plan-TEAM-enroll-client-key); with neither, refused.
//
// The binding's key is GetClientBindingKey's, which refuses an older
// generation's. The owner row's column is MG1's, so a binary without MG1 reads
// the binding alone, and a client with no binding has no key there.
func ClientProofPublicKey(ctx context.Context, clientId server.Id) (ed25519.PublicKey, error) {
	clientPublicKey, err := GetClientBindingKey(ctx, clientId)
	if err == nil && len(clientPublicKey) == ed25519.PublicKeySize {
		return ed25519.PublicKey(clientPublicKey), nil
	}
	if !server.SidecarMg1Linked() {
		return nil, ErrClientProofKeyNotBound
	}
	enrolledKey, found := getEnrolledClientPublicKey(ctx, clientId)
	if !found || len(enrolledKey) != ed25519.PublicKeySize {
		return nil, ErrClientProofKeyNotBound
	}
	return ed25519.PublicKey(enrolledKey), nil
}

// The client_public_key its enrollment stored, false for none or an error.
func getEnrolledClientPublicKey(ctx context.Context, clientId server.Id) (clientPublicKey []byte, found bool) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT client_public_key FROM network_client_owner WHERE client_id = $1 AND client_public_key IS NOT NULL`,
				clientId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&clientPublicKey))
					found = true
				}
			})
		})
	})
	return
}

// The client_public_key its enrollment stored, read in tx, nil for none: a
// binding of the client must name exactly it (plan-TEAM-enroll-client-key).
func getEnrolledClientPublicKeyInTx(ctx context.Context, tx server.PgTx, clientId server.Id) []byte {
	if !server.SidecarMg1Linked() {
		return nil
	}
	var clientPublicKey []byte
	result, err := tx.Query(
		ctx,
		`SELECT client_public_key FROM network_client_owner WHERE client_id = $1`,
		clientId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&clientPublicKey))
		}
	})
	return clientPublicKey
}

// §7.8's enrollment (R-TEAM-5, R-TEAM-6, D-TEAM-5, D-TEAM-22, I-6, I-13):
// POST /network/enroll creates a sidecar's top-level client from an auth key,
// with no credential but the key. The mint is the key creator's, through
// authNetworkClientWithParentState's path, and in its transaction it spends one
// of the key's uses (ConsumeAuthKeyInTx), writes the client's owner row and,
// for an ephemeral key, its lease and close capability.
//
//	marks     an enrolled client is a sidecar's: admin_refused and strict_exp
//	          whatever the request says (I-6, D-TEAM-14), so outside
//	          sidecar.yml's preview_networks it is refused (D-TRUST-3)
//	owner     untagged: the key's creator owns it, its member in a personal
//	          network; tagged: no owner, the creator its sponsor (§7.7)
//	genesis   a tagged or ephemeral enrollment before the network's base 1
//	          answers no_genesis, so base 1's signer is never one (§7.9's who)
//	bundle    the request has no field for a key's #<bundle>: a bearer holding
//	          "#", or a urn_ API key, is refused, its text never logged or
//	          answered back (D-TEAM-22)
//	admission a pre-approved key's enrollment may carry its device's own
//	          SidecarAdmit, signed by the key's Delegation, which api appends
//	          to the log before the mint (sidecar_admission_model.go,
//	          TEAM-M2-16)
//
// The client starts unadmitted (approval_state pending): admission is the
// config repo's SidecarAdmit, never api's (§7.9). With an admission that
// landed, the answer says approved, naming what the log already holds.

// The approval state of a client an enrollment mints with no admission:
// admission happens in the config repo, not here.
const NetworkEnrollApprovalStatePending = "pending"

// The approval state of a client whose enrollment carried an admission that
// landed in the log at the tip.
const NetworkEnrollApprovalStateApproved = "approved"

// The codes of POST /network/enroll's refusals, as SidecarApi reads them
// (sidecar's identity_credential.go and sidecartest's fake api).
const (
	NetworkEnrollCodeBadRequest     = "bad_request"
	NetworkEnrollCodeAuthKeyInvalid = "auth_key_invalid"
	// D-TRUST-3's preview refusal (questions/core-m1-08-enroll-preview-code.md)
	NetworkEnrollCodeNotPreview = "not_preview"
	// §7.9: a tagged or ephemeral enrollment before base 1
	NetworkEnrollCodeNoGenesis   = "no_genesis"
	NetworkEnrollCodeRefused     = "refused"
	NetworkEnrollCodeUnavailable = "unavailable"
)

// The longest device_spec an enrollment takes.
const networkEnrollDeviceSpecMaxLength = 256

// What POST /network/enroll takes (§7.8). It has no field for an auth key's
// #<bundle>, and prints without the key (String).
type NetworkEnrollArgs struct {
	// The key's bearer, ur-authkey-<id>-<secret> or <id>-<secret>. One holding
	// "#" or naming a urn_ API key is refused, its text never logged or
	// answered back.
	AuthKey string `json:"auth_key"`
	// The new client's Ed25519 public key, 32 bytes, which a strict_exp proof
	// is checked against until the client binds (plan-TEAM-enroll-client-key).
	ClientPublicKey []byte `json:"client_public_key"`
	DeviceSpec      string `json:"device_spec"`
	// §20.8's declaration.
	DeclaresNoEgress bool `json:"declares_no_egress,omitempty"`
	// §7.4's marks, which a sidecar always sends; an enrolled client carries
	// both whatever these say.
	AdminRefused bool `json:"admin_refused,omitempty"`
	StrictExp    bool `json:"strict_exp,omitempty"`
	// A pre-approved key's admission: the device's own Signed SidecarAdmit,
	// verbatim, which the key's Delegation signed (§7.8). api appends it to
	// the log before the mint.
	Admission []byte `json:"admission,omitempty"`
}

// Never the key.
func (self *NetworkEnrollArgs) String() string {
	return "NetworkEnrollArgs{device_spec " + strconv.Quote(self.DeviceSpec) + "}"
}

// As String, for %#v.
func (self *NetworkEnrollArgs) GoString() string {
	return self.String()
}

// What POST /network/enroll answers (§7.8). It prints without the token or
// the capability (String).
type NetworkEnrollResult struct {
	ByClientJwt   string    `json:"by_client_jwt"`
	ClientId      server.Id `json:"client_id"`
	ApprovalState string    `json:"approval_state"`
	// Always true: a sidecar keeps its keychain only when this confirms it.
	AdminRefused bool `json:"admin_refused"`
	// An ephemeral key's mint returns its close capability once, here.
	Close string `json:"close,omitempty"`
}

// Never the token or the capability.
func (self *NetworkEnrollResult) String() string {
	return "NetworkEnrollResult{client " + self.ClientId.String() + "}"
}

// As String, for %#v.
func (self *NetworkEnrollResult) GoString() string {
	return self.String()
}

// An enrollment's refusal: RaiseHttpError answers its status with
// {"error": message, "code": code}, and its text holds no request value.
type NetworkEnrollRefusal struct {
	Status  int
	Code    string
	Message string
	// for a 429, whole seconds until the caller may try again
	RetryAfter int
}

func (self *NetworkEnrollRefusal) Error() string {
	return fmt.Sprintf("%d %s", self.Status, self.Message)
}

// The coded body, the server's refusal shape.
func (self *NetworkEnrollRefusal) HttpErrorResultBody() any {
	return map[string]string{
		"error": self.Message,
		"code":  self.Code,
	}
}

// The Retry-After header's seconds; zero for none.
func (self *NetworkEnrollRefusal) RetryAfterSeconds() int {
	return self.RetryAfter
}

func newNetworkEnrollRefusal(status int, code string, message string) *NetworkEnrollRefusal {
	return &NetworkEnrollRefusal{Status: status, Code: code, Message: message}
}

// What an enrollment adds to authNetworkClientWithParentState's mint.
type networkClientEnrollment struct {
	// the bearer, spent in the mint's transaction
	bearer      string
	authKeyId   server.Id
	declaresKey []byte
	// set in the mint's transaction: the key it spent, or why it spent none
	authKey *NetworkAuthKey
	refusal error
}

// The owner row of the client the enrollment mints, from the key it spent.
func (self *networkClientEnrollment) ownerRow(marks clientMarks) *clientOwnerInsert {
	authKeyId := self.authKey.AuthKeyId
	creatorUserId := self.authKey.CreatorUserId
	row := &clientOwnerInsert{
		authKeyId:       &authKeyId,
		tagged:          0 < len(self.authKey.TagIds),
		ephemeral:       self.authKey.Ephemeral,
		marks:           marks,
		clientPublicKey: self.declaresKey,
	}
	if row.tagged {
		// a tagged client has no owner; its admitting member sponsors it (§7.7)
		row.sponsorUserId = &creatorUserId
	} else {
		// the creator's own device; its member is the creator's user in a
		// personal network (§7.5)
		row.ownerUserId = &creatorUserId
		row.memberId = &creatorUserId
	}
	return row
}

// Called in an enrollment's mint transaction just before it spends its key:
// the barrier racing enrollments wait at. nil in production.
var Testing_EnrollBeforeConsume func()

// What an enrollment reads of its key before the mint: the network and creator
// the mint is for, and whether the key is tagged or ephemeral. No lock and no
// secret check: the mint's ConsumeAuthKeyInTx is the authority, and a key that
// moved on meanwhile refuses there.
type networkEnrollKey struct {
	networkId     server.Id
	networkName   string
	creatorUserId server.Id
	tagged        bool
	ephemeral     bool
	preApproved   bool
}

func readNetworkEnrollKey(ctx context.Context, authKeyId server.Id) (key *networkEnrollKey) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_auth_key.network_id,
					network.network_name,
					network_auth_key.creator_user_id,
					0 < cardinality(network_auth_key.tag_ids),
					network_auth_key.ephemeral,
					network_auth_key.pre_approved
				FROM network_auth_key
				INNER JOIN network ON network.network_id = network_auth_key.network_id
				WHERE network_auth_key.auth_key_id = $1
			`,
			authKeyId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				key = &networkEnrollKey{}
				server.Raise(result.Scan(&key.networkId, &key.networkName, &key.creatorUserId, &key.tagged, &key.ephemeral, &key.preApproved))
			}
		})
	})
	return
}

// Whether the network's authorization log holds base 1: what a tagged or
// ephemeral enrollment waits for (§7.9's enroll row). A test replaces it.
var networkEnrollHasGenesis = func(ctx context.Context, networkId server.Id) (bool, error) {
	tip, err := ConfigTip(ctx, networkId)
	if err != nil {
		return false, err
	}
	return tip != nil && 1 <= tip.GetVersion(), nil
}

// POST /network/enroll (§7.8): mints the sidecar's top-level client of the
// key's network, as the key's creator, at clientAddress. Every refusal is a
// *NetworkEnrollRefusal or a *QuotaRefusalError, and none holds the key's
// text; any other error is internal.
func EnrollNetworkClient(ctx context.Context, enroll *NetworkEnrollArgs, clientAddress string) (*NetworkEnrollResult, error) {
	if !server.SidecarMg1Linked() {
		return nil, newNetworkEnrollRefusal(http.StatusServiceUnavailable, NetworkEnrollCodeUnavailable, "Enrollment is not available yet.")
	}
	authKeyId, _, err := ParseAuthKeyBearer(enroll.AuthKey)
	switch {
	case errors.Is(err, ErrAuthKeyHoldsBundle), errors.Is(err, ErrAuthKeyIsApiKey):
		return nil, newNetworkEnrollRefusal(http.StatusBadRequest, NetworkEnrollCodeBadRequest, "The auth key is not an auth key's bearer.")
	case err != nil:
		return nil, newNetworkEnrollRefusal(http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid, networkEnrollAuthKeyInvalidMessage)
	}
	if len(enroll.ClientPublicKey) != ed25519.PublicKeySize || enroll.DeviceSpec == "" || networkEnrollDeviceSpecMaxLength < len(enroll.DeviceSpec) {
		return nil, newNetworkEnrollRefusal(http.StatusBadRequest, NetworkEnrollCodeBadRequest, "The client key or device is missing.")
	}
	var admission *networkEnrollAdmission
	if 0 < len(enroll.Admission) {
		admission, err = parseNetworkEnrollAdmission(enroll.Admission)
		if err != nil {
			return nil, newNetworkEnrollRefusal(http.StatusBadRequest, NetworkEnrollCodeBadRequest, "The admission is malformed.")
		}
	}

	key := readNetworkEnrollKey(ctx, authKeyId)
	if key == nil {
		return nil, newNetworkEnrollRefusal(http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid, networkEnrollAuthKeyInvalidMessage)
	}
	if !SidecarPreviewAllowed(key.networkId) {
		return nil, newNetworkEnrollRefusal(http.StatusForbidden, NetworkEnrollCodeNotPreview, SidecarPreviewRefusedMessage)
	}
	if key.tagged || key.ephemeral {
		hasGenesis, err := networkEnrollHasGenesis(ctx, key.networkId)
		if err != nil {
			return nil, newNetworkEnrollRefusal(http.StatusServiceUnavailable, NetworkEnrollCodeUnavailable, "The network's log is unavailable.")
		}
		if !hasGenesis {
			return nil, newNetworkEnrollRefusal(http.StatusForbidden, NetworkEnrollCodeNoGenesis, "The network has no genesis yet.")
		}
	}
	if refusal := SidecarClientsExceeded(ctx, key.networkId); refusal != nil {
		return nil, NewQuotaRefusalError(refusal)
	}
	approvalState := NetworkEnrollApprovalStatePending
	if admission != nil {
		if _, err := landNetworkEnrollAdmission(ctx, enroll.AuthKey, key, admission); err != nil {
			return nil, err
		}
		approvalState = NetworkEnrollApprovalStateApproved
	}

	creatorSession := session.NewLocalClientSession(
		ctx,
		clientAddress,
		session.NewByJwt(key.networkId, key.creatorUserId, key.networkName, false, false),
	)
	defer creatorSession.Cancel()
	enrollment := &networkClientEnrollment{
		bearer:      enroll.AuthKey,
		authKeyId:   authKeyId,
		declaresKey: bytes.Clone(enroll.ClientPublicKey),
	}
	result, err := authNetworkClientWithParentState(&AuthNetworkClientArgs{
		DeviceSpec:       enroll.DeviceSpec,
		AdminRefused:     true,
		StrictExp:        true,
		DeclaresNoEgress: enroll.DeclaresNoEgress,
		enrollment:       enrollment,
	}, creatorSession, nil, false)
	if err != nil {
		return nil, err
	}
	if enrollment.refusal != nil {
		return nil, newNetworkEnrollRefusal(http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid, networkEnrollAuthKeyInvalidMessage)
	}
	if result == nil || result.Error != nil {
		switch {
		case result != nil && result.Error.ClientLimitExceeded:
			return nil, &NetworkEnrollRefusal{Status: http.StatusTooManyRequests, Code: NetworkEnrollCodeRefused, Message: "The network is at its client limit."}
		case result != nil && result.Error.Message == SidecarPreviewRefusedMessage:
			return nil, newNetworkEnrollRefusal(http.StatusForbidden, NetworkEnrollCodeNotPreview, SidecarPreviewRefusedMessage)
		}
		return nil, newNetworkEnrollRefusal(http.StatusForbidden, NetworkEnrollCodeRefused, "The enrollment is refused.")
	}
	if result.ByClientJwt == nil || result.ClientId == nil || !result.AdminRefused || !result.StrictExp {
		return nil, errors.New("an enrollment minted no sidecar client")
	}
	return &NetworkEnrollResult{
		ByClientJwt:   *result.ByClientJwt,
		ClientId:      *result.ClientId,
		ApprovalState: approvalState,
		AdminRefused:  true,
		Close:         result.Close,
	}, nil
}

// Whether the client's owner row marks it tagged or ephemeral, neither of
// which may sign its network's genesis (§7.9's who). False for a client with no
// owner row and in a binary that does not link MG0; an error is the caller's to
// refuse on.
func ClientTaggedOrEphemeral(ctx context.Context, clientId server.Id) (taggedOrEphemeral bool, returnErr error) {
	if !server.SidecarMg0Linked() {
		return false, nil
	}
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT tagged OR ephemeral FROM network_client_owner WHERE client_id = $1`,
				clientId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&taggedOrEphemeral))
				}
			})
		})
	}); r != nil {
		return false, fmt.Errorf("client owner: %v", r)
	}
	return
}

// Replaces the check of a network's base 1, returning a restore function, for
// tests that enroll a tagged or ephemeral key with no log.
func Testing_SetNetworkEnrollHasGenesis(hasGenesis func(ctx context.Context, networkId server.Id) (bool, error)) func() {
	previous := networkEnrollHasGenesis
	networkEnrollHasGenesis = hasGenesis
	return func() {
		networkEnrollHasGenesis = previous
	}
}

// §7.4's forwarding client (TEAM-M8-02; owner decision 197; DESIGN-DETAILS
// 4.2.6's S5): a sidecar S mints F for its app's VPN component with S's own
// client token and S's DPoP proof, naming the component's ClientKey.
//
//	mark     forward_only, which sets admin_refused and strict_exp too and
//	         which F's children, its window clients, inherit; MG8's column
//	source   S's own client, a bound sidecar client of the newest generation:
//	         the bearer is that client and a strict_exp one, so the proof gate
//	         checked its proof (api/route_authz.go's strictExpProofRoutes)
//	key      client_public_key, 32 B, stored in F's owner row (MG1's column):
//	         with no binding it is the key F's own proofs are checked against
//	         (ClientProofPublicKey). Taken only for a new forwarding client
//	repeat   a mint naming a key an active forwarding child of S's client
//	         already holds reissues that client, so a lost answer mints no
//	         second F
//	exchange only the newest active forwarding child of S's current client
//	         speaks for S (ForwardingClientSidecar), so a setup that supersedes
//	         S's forwarding Delegation with other keys, and so mints a new F,
//	         ends the old F's exchange; its window clients never exchange

// The client admin settings' bound, for a forwarding answer too.
var clientForwardOnlyAnswers = newBoundedClientMap[bool](DefaultClientAdminSettings().CachedClientMaxCount)

// Whether the client is a forwarding client or a child of one: its owner row's
// forward_only. False in a binary that does not link MG8, which can mint
// none. An error for a read that failed or a client its network does not hold,
// which every caller refuses on. Each answer is immutable, so it is kept for as
// long as this process can hold it.
func ClientForwardOnly(ctx context.Context, networkId server.Id, clientId server.Id) (bool, error) {
	if !server.SidecarMg8Linked() {
		return false, nil
	}
	if forwardOnly, ok := clientForwardOnlyAnswers.Load(clientId); ok {
		return forwardOnly, nil
	}
	found := false
	forwardOnly := false
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT COALESCE(network_client_owner.forward_only, false)
					FROM network_client
					LEFT JOIN network_client_owner ON network_client_owner.client_id = network_client.client_id
					WHERE
						network_client.client_id = $1 AND
						network_client.network_id = $2
				`,
				clientId,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&forwardOnly))
					found = true
				}
			})
		})
	}); r != nil {
		return false, fmt.Errorf("client forward_only: %v", r)
	}
	if !found {
		return false, errors.New("client forward_only: no such client in the network")
	}
	clientForwardOnlyAnswers.Store(clientId, forwardOnly)
	return forwardOnly, nil
}

// Drops this process's forwarding answers, for a test that writes a client's
// rows directly after the client was asked about.
func Testing_ForgetClientForwardOnly() {
	clientForwardOnlyAnswers.Clear()
}

// What a forwarding mint is refused with.
const (
	clientForwardOnlyMinterMessage  = "A forwarding client is minted by its sidecar's own client, with its proof."
	clientForwardOnlyKeyMessage     = "A forwarding client needs its 32-byte client_public_key."
	clientForwardOnlyUnboundMessage = "A forwarding client's source must be a bound sidecar client."
	clientPublicKeyMarkMessage      = "A client_public_key is taken only for a new forwarding client."
	clientPublicKeyMismatchMessage  = "The client_public_key is not the client's."
)

// What a mint's request asks of forward_only before its transaction (§7.4's
// mark row), with no lock held: a message to refuse with, or, for a repeat
// whose key an active forwarding child of the source already holds, that
// client, which the mint then reissues. A child of a forwarding client inherits
// the mark and names no key. A read that fails is the caller's error.
func checkForwardOnlyMint(ctx context.Context, authClient *AuthNetworkClientArgs, clientSession *session.ClientSession) (reuseClientId *server.Id, message string, returnErr error) {
	if !authClient.ForwardOnly {
		if authClient.ClientPublicKey != nil {
			return nil, clientPublicKeyMarkMessage, nil
		}
		return nil, "", nil
	}
	if authClient.ClientId != nil {
		// a reissue: its marks and key are checked against the client's own in
		// its transaction
		return nil, "", nil
	}
	sourceClientId := authClient.SourceClientId
	callerClientId := clientSession.ByJwt.ClientId
	if sourceClientId == nil || callerClientId == nil || *sourceClientId != *callerClientId || !clientSession.ByJwt.StrictExp {
		return nil, clientForwardOnlyMinterMessage, nil
	}
	networkId := clientSession.ByJwt.NetworkId
	sourceForwardOnly, err := ClientForwardOnly(ctx, networkId, *sourceClientId)
	if err != nil {
		return nil, "", err
	}
	if sourceForwardOnly {
		// F's own child, a window client: the mark is inherited
		if authClient.ClientPublicKey != nil {
			return nil, clientPublicKeyMarkMessage, nil
		}
		return nil, "", nil
	}
	if len(authClient.ClientPublicKey) != ed25519.PublicKeySize {
		return nil, clientForwardOnlyKeyMessage, nil
	}
	if _, err := GetClientBindingKey(ctx, *sourceClientId); err != nil {
		return nil, clientForwardOnlyUnboundMessage, nil
	}
	existing, err := getForwardOnlyChildByKey(ctx, networkId, *sourceClientId, authClient.ClientPublicKey)
	if err != nil {
		return nil, "", err
	}
	return existing, "", nil
}

// The key a new client's owner row stores: a new forwarding client's, nil for
// every other client, a forwarding client's child included.
func forwardOnlyMintKey(authClient *AuthNetworkClientArgs, parentMarks clientMarks) []byte {
	if !authClient.ForwardOnly || parentMarks.forwardOnly {
		return nil
	}
	return bytes.Clone(authClient.ClientPublicKey)
}

// A reissue naming a key must name the client's own: nil when it names none or
// the client's, else the message to refuse with.
func refuseReissuedClientPublicKeyInTx(ctx context.Context, tx server.PgTx, authClient *AuthNetworkClientArgs) string {
	if authClient.ClientPublicKey == nil {
		return ""
	}
	if !bytes.Equal(getEnrolledClientPublicKeyInTx(ctx, tx, *authClient.ClientId), authClient.ClientPublicKey) {
		return clientPublicKeyMismatchMessage
	}
	return ""
}

// The newest active forwarding child of sourceClientId that holds key, nil for
// none. The source's children share its device, so the read is the device's.
func getForwardOnlyChildByKey(ctx context.Context, networkId server.Id, sourceClientId server.Id, key []byte) (clientId *server.Id, returnErr error) {
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT network_client.client_id
					FROM network_client
					INNER JOIN network_client_owner ON network_client_owner.client_id = network_client.client_id
					WHERE
						network_client.device_id = (
							SELECT device_id FROM network_client WHERE client_id = $2 AND network_id = $1
						) AND
						network_client.network_id = $1 AND
						network_client.source_client_id = $2 AND
						network_client.active = true AND
						network_client_owner.forward_only = true AND
						network_client_owner.client_public_key = $3
					ORDER BY network_client.create_time DESC, network_client.client_id DESC
					LIMIT 1
				`,
				networkId,
				sourceClientId,
				key,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var found server.Id
					server.Raise(result.Scan(&found))
					clientId = &found
				}
			})
		})
	}); r != nil {
		return nil, fmt.Errorf("forwarding client by key: %v", r)
	}
	return
}

// The sidecar a forwarding client speaks for at the exchange (§7.8's
// forwarding rule): S, when the client is the newest active forwarding child
// of S's client of the newest generation, bound to S. Nil for any other
// client: an older forwarding client of S's client, which a later setup's
// supersedes, a forwarding client's own child, or one whose source no longer
// is S's current client. An error for a read that failed.
func ForwardingClientSidecar(ctx context.Context, networkId server.Id, clientId server.Id) (sidecarId *server.Id, returnErr error) {
	var sourceClientId *server.Id
	newest := false
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT
						client.source_client_id,
						client.client_id = (
							SELECT sibling.client_id
							FROM network_client sibling
							INNER JOIN network_client_owner sibling_owner ON sibling_owner.client_id = sibling.client_id
							WHERE
								sibling.device_id = client.device_id AND
								sibling.network_id = client.network_id AND
								sibling.source_client_id = client.source_client_id AND
								sibling.active = true AND
								sibling_owner.forward_only = true
							ORDER BY sibling.create_time DESC, sibling.client_id DESC
							LIMIT 1
						)
					FROM network_client client
					INNER JOIN network_client_owner owner ON owner.client_id = client.client_id
					WHERE
						client.client_id = $1 AND
						client.network_id = $2 AND
						client.active = true AND
						owner.forward_only = true
				`,
				clientId,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					var isNewest *bool
					server.Raise(result.Scan(&sourceClientId, &isNewest))
					newest = isNewest != nil && *isNewest
				}
			})
		})
	}); r != nil {
		return nil, fmt.Errorf("forwarding client: %v", r)
	}
	if sourceClientId == nil || !newest {
		return nil, nil
	}
	// a failed read answers as no binding, which refuses
	if _, err := GetClientBindingKey(ctx, *sourceClientId); err != nil {
		return nil, nil
	}
	view := GetSidecarIdentityByClient(ctx, networkId, *sourceClientId)
	if view == nil {
		return nil, nil
	}
	sidecar := view.SidecarId
	return &sidecar, nil
}
