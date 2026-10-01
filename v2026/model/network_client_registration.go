package model

// A persisted request key selects exactly one server-issued client/device.
// Creation and the key binding share a transaction; replay never recreates a
// removed identity or borrows authority from a different request or principal.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

const NetworkClientRegistrationSchema = "urnetwork-client-registration-v1"

// This intentionally excludes caller-selected client/device IDs and proxy or
// payment operations. The opaque scope is immutable caller-owned provenance.
type RegisterNetworkClientArgs struct {
	Schema         string `json:"schema"`
	RegistrationId string `json:"registration_id"`
	ScopeSha256    string `json:"scope_sha256"`
	Description    string `json:"description"`
	DeviceSpec     string `json:"device_spec"`
}

// A versioned operation must not silently discard a caller's identity fields.
// Legacy creation keeps its existing permissive wire contract separately.
func (self *RegisterNetworkClientArgs) UnmarshalJSON(raw []byte) error {
	// The fixed tag grammar precedes Go's case-insensitive struct matching.
	// Track decoded keys so escaped duplicates cannot overwrite ownership.
	keys := json.NewDecoder(bytes.NewReader(raw))
	start, err := keys.Token()
	if err != nil || start != json.Delim('{') {
		return errors.New("registration request must be one JSON object")
	}
	seen := map[string]bool{}
	for keys.More() {
		token, err := keys.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return errors.New("registration request has an invalid or duplicate field")
		}
		switch name {
		case "schema", "registration_id", "scope_sha256", "description", "device_spec":
		default:
			return errors.New("registration request has an unknown or noncanonical field")
		}
		seen[name] = true
		var fieldValue *string
		if err := keys.Decode(&fieldValue); err != nil {
			return err
		}
		if fieldValue == nil {
			return errors.New("registration request fields must be strings")
		}
	}
	if _, err := keys.Token(); err != nil {
		return err
	}
	type fields RegisterNetworkClientArgs
	var value fields
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("registration request contains trailing data")
	}
	*self = RegisterNetworkClientArgs(value)
	return nil
}

// The echo lets a caller authenticate the exact operation before installing
// its credential. Server authentication, not the opaque key, grants permission.
type RegisterNetworkClientResult struct {
	Schema         string                      `json:"schema"`
	RegistrationId string                      `json:"registration_id"`
	RequestSha256  string                      `json:"request_sha256"`
	ClientId       *server.Id                  `json:"client_id,omitempty"`
	DeviceId       *server.Id                  `json:"device_id,omitempty"`
	ByClientJwt    *string                     `json:"by_client_jwt,omitempty"`
	Error          *RegisterNetworkClientError `json:"error,omitempty"`
}

// Codes are closed protocol outcomes. A rejected binding remains a tombstone;
// neither a fresh network token nor a process restart resets its identity.
type RegisterNetworkClientError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type networkClientRegistrationOwner struct {
	request       RegisterNetworkClientArgs
	requestHash   string
	authorityHash string
	deviceId      server.Id
	code          string
}

// Canonical hex excludes an empty/zero identifier and alternate encodings.
func validNetworkClientRegistrationHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value) && value != strings.Repeat("0", 64)
}

// Only an authenticated network session can create or resume this operation.
// Invalid shapes are rejected before any database mutation.
func RegisterNetworkClient(args *RegisterNetworkClientArgs, clientSession *session.ClientSession) (*RegisterNetworkClientResult, error) {
	if args == nil || args.Schema != NetworkClientRegistrationSchema || !validNetworkClientRegistrationHash(args.RegistrationId) || !validNetworkClientRegistrationHash(args.ScopeSha256) || len(args.Description) > 1024 || len(args.DeviceSpec) > 4096 {
		return &RegisterNetworkClientResult{Error: &RegisterNetworkClientError{Code: "invalid_request", Message: "Registration request is incomplete or unsupported."}}, nil
	}
	if clientSession == nil || clientSession.ByJwt == nil || clientSession.ByJwt.ClientId != nil || clientSession.ByJwt.DeviceId != nil || clientSession.ByJwt.NetworkId == (server.Id{}) || clientSession.ByJwt.UserId == (server.Id{}) {
		return &RegisterNetworkClientResult{Error: &RegisterNetworkClientError{Code: "network_authority_required", Message: "Registration requires an authenticated network session."}}, nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	owner := &networkClientRegistrationOwner{request: *args, requestHash: hex.EncodeToString(digest[:])}
	authenticated, err := authNetworkClient(&AuthNetworkClientArgs{Description: args.Description, DeviceSpec: args.DeviceSpec}, clientSession, owner)
	if err != nil {
		return nil, err
	}
	result := &RegisterNetworkClientResult{Schema: NetworkClientRegistrationSchema, RegistrationId: args.RegistrationId, RequestSha256: owner.requestHash}
	if authenticated == nil {
		return nil, errors.New("registration omitted its authenticated client result")
	}
	if authenticated.Error != nil {
		code := owner.code
		if code == "" {
			code = "authentication_refused"
			if authenticated.Error.ClientLimitExceeded {
				code = "client_limit"
			}
		}
		result.Error = &RegisterNetworkClientError{Code: code, Message: authenticated.Error.Message}
		return result, nil
	}
	if authenticated.ClientId == nil || authenticated.ByClientJwt == nil || owner.deviceId == (server.Id{}) {
		return nil, errors.New("registration omitted its bound client/device identity")
	}
	deviceId := owner.deviceId
	result.ClientId, result.DeviceId, result.ByClientJwt = authenticated.ClientId, &deviceId, authenticated.ByClientJwt
	return result, nil
}

// Serialize registration allocation within the authenticated network. A
// duplicate checks the original request before reading the actual active row.
// The lock is transaction-owned, including cancellation and rollback.
func (self *networkClientRegistrationOwner) resumeInTx(tx server.PgTx, clientSession *session.ClientSession, isPro bool, roles []string, principal string) (*AuthNetworkClientResult, bool) {
	ctx, claims := clientSession.Ctx, clientSession.ByJwt
	self.deviceId, self.code = server.Id{}, ""
	roles = slices.Clone(roles)
	slices.Sort(roles)
	authority, err := json.Marshal(struct {
		UserId    server.Id `json:"user_id"`
		Roles     []string  `json:"roles"`
		Principal string    `json:"principal"`
	}{UserId: claims.UserId, Roles: roles, Principal: principal})
	server.Raise(err)
	digest := sha256.Sum256(authority)
	self.authorityHash = hex.EncodeToString(digest[:])
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "network-client-registration/v1:"+claims.NetworkId.String()))
	var registrationId, requestHash, authorityHash string
	var userId, clientId, deviceId server.Id
	err = tx.QueryRow(ctx, `SELECT registration_id, request_sha256, authority_sha256, user_id, client_id, device_id FROM network_client_registration WHERE network_id=$1 AND (registration_id=$2 OR scope_sha256=$3) ORDER BY registration_id LIMIT 1`, claims.NetworkId, self.request.RegistrationId, self.request.ScopeSha256).Scan(&registrationId, &requestHash, &authorityHash, &userId, &clientId, &deviceId)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false
	}
	server.Raise(err)
	refused := func(code, message string) (*AuthNetworkClientResult, bool) {
		self.code = code
		return &AuthNetworkClientResult{Error: &AuthNetworkClientError{Message: message}}, true
	}
	if registrationId != self.request.RegistrationId || requestHash != self.requestHash || authorityHash != self.authorityHash || userId != claims.UserId {
		return refused("registration_conflict", "Registration is bound to a different request or authority.")
	}
	var active bool
	var actualDevice server.Id
	err = tx.QueryRow(ctx, `SELECT c.active, c.device_id FROM network_client c JOIN device d ON d.device_id=c.device_id AND d.network_id=c.network_id WHERE c.client_id=$1 AND c.network_id=$2 FOR UPDATE OF c, d`, clientId, claims.NetworkId).Scan(&active, &actualDevice)
	if errors.Is(err, pgx.ErrNoRows) {
		return refused("identity_unavailable", "The registered client or device is no longer available.")
	}
	server.Raise(err)
	if !active || actualDevice != deviceId {
		return refused("identity_unavailable", "The registered identity is inactive or differs from its original device.")
	}
	var storedPrincipal string
	server.Raise(tx.QueryRow(ctx, `SELECT principal FROM network_client WHERE client_id=$1`, clientId).Scan(&storedPrincipal))
	storedRoles := []string{}
	rows, err := tx.Query(ctx, `SELECT role FROM network_client_role WHERE client_id=$1 ORDER BY role`, clientId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var role string
			server.Raise(rows.Scan(&role))
			storedRoles = append(storedRoles, role)
		}
	})
	if storedPrincipal != principal || !slices.Equal(storedRoles, roles) {
		return refused("identity_unavailable", "The registered identity no longer matches its original role authority.")
	}
	credential := jwt.NewByJwtWithCreateTime(claims.NetworkId, claims.UserId, claims.NetworkName, claims.CreateTime, claims.GuestMode, isPro).Client(deviceId, clientId)
	credential.Roles, credential.Principal = storedRoles, storedPrincipal
	signed := credential.Sign()
	self.deviceId = deviceId
	return &AuthNetworkClientResult{ByClientJwt: &signed, ClientId: &clientId}, true
}

// Called in the existing allocation transaction after both rows exist. A
// rollback removes the complete operation; a lost response leaves this binding.
func (self *networkClientRegistrationOwner) bindInTx(tx server.PgTx, clientSession *session.ClientSession, clientId, deviceId server.Id) {
	server.RaisePgResult(tx.Exec(clientSession.Ctx, `INSERT INTO network_client_registration (network_id,registration_id,request_sha256,scope_sha256,authority_sha256,user_id,client_id,device_id,create_time) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, clientSession.ByJwt.NetworkId, self.request.RegistrationId, self.requestHash, self.request.ScopeSha256, self.authorityHash, clientSession.ByJwt.UserId, clientId, deviceId, server.NowUtc()))
	self.deviceId = deviceId
}
