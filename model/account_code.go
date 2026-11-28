package model

// Short-lived confirmation codes sent to an account's own email addresses: the
// two codes of an email change (one to the current address, one to the new
// one) and the code that confirms a payout wallet disconnect. A request is one
// Redis hash under a key of its owner and purpose, with a TTL to its expiry,
// so nothing outlives it and no migration is needed. A new request of the same
// key replaces a waiting one, and with it every code it sent.
//
// A code is never stored: each slot keeps an hmac of the code, keyed by the
// password pepper and bound to the purpose, the user, the request and the
// slot, so a code of one request, slot or account never confirms another, and
// the store holds nothing that confirms anything.
//
// Every try runs in one script (accountCodeTryScript): it compares each
// candidate with its slot, marks the right ones confirmed, and counts a try
// with any wrong candidate before it answers, so concurrent tries cannot pass
// the try limit together. The try that reaches the limit deletes the request.
// A request is checked by its id, so a try that races a replacement never
// confirms the replacement.
//
// The functions raise Redis failures; the routes answer them as errors, and a
// lost request only means the person asks for new codes.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The fields every request hash carries beside its own.
const (
	accountCodeIdField      = "id"
	accountCodeTriesField   = "tries"
	accountCodeExpiresField = "expires_ms"
	// the slot's hmac and whether it is confirmed: mac.0, ok.0, mac.1, ...
	accountCodeMacFieldPrefix       = "mac."
	accountCodeConfirmedFieldPrefix = "ok."
)

// The most slots one request holds.
const accountCodeMaxSlots = 4

// The key of a request: one per owner and purpose. The hash tag is the whole
// owner, so the request's single key always maps to one slot of the cluster.
func accountCodeKey(purpose string, userId server.Id) string {
	return fmt.Sprintf("{account_code:%s:%s}request", purpose, userId)
}

// The hmac a slot keeps for its code, keyed with the password pepper.
func accountCodeMac(purpose string, userId server.Id, requestId server.Id, slot int, code string) string {
	return accountCodeMacWithKey(passwordPepper(), purpose, userId, requestId, slot, code)
}

func accountCodeMacWithKey(key []byte, purpose string, userId server.Id, requestId server.Id, slot int, code string) string {
	mac := hmac.New(sha256.New, key)
	for _, part := range []string{"account-code", purpose, userId.String(), requestId.String(), strconv.Itoa(slot), code} {
		_, err := mac.Write([]byte(part))
		server.Raise(err)
		_, err = mac.Write([]byte{0})
		server.Raise(err)
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// A code as a person typed it, as it is compared: spaces and dashes dropped
// (a code can be pasted grouped). The sdk normalizes it the same way.
func normalizeAccountCode(code string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '-' {
			return -1
		}
		return r
	}, code)
}

// A new request: its id, a fresh numeric code per slot, and its own fields.
type accountCodeRequest struct {
	Purpose string
	UserId  server.Id
	Id      server.Id
	// the codes, one per slot, to send; never stored
	Codes      []string
	ExpireTime time.Time
	Fields     map[string]string
}

// Makes a request with `slots` fresh codes, to put with putAccountCodeRequest.
func newAccountCodeRequest(purpose string, userId server.Id, slots int, expireTime time.Time, fields map[string]string) *accountCodeRequest {
	if slots < 1 || accountCodeMaxSlots < slots {
		panic(fmt.Errorf("account code request with %d slots", slots))
	}
	codes := make([]string, slots)
	for i := range codes {
		codes[i] = createVerifyCode(VerifyCodeNumeric)
	}
	return &accountCodeRequest{
		Purpose:    purpose,
		UserId:     userId,
		Id:         server.NewId(),
		Codes:      codes,
		ExpireTime: expireTime,
		Fields:     fields,
	}
}

// Replaces the owner's waiting request of the purpose with this one, which
// expires at its expire time.
func putAccountCodeRequest(ctx context.Context, request *accountCodeRequest) {
	key := accountCodeKey(request.Purpose, request.UserId)
	values := map[string]any{
		accountCodeIdField:      request.Id.String(),
		accountCodeTriesField:   0,
		accountCodeExpiresField: request.ExpireTime.UnixMilli(),
	}
	for name, value := range request.Fields {
		values[name] = value
	}
	for slot, code := range request.Codes {
		values[accountCodeMacFieldPrefix+strconv.Itoa(slot)] = accountCodeMac(request.Purpose, request.UserId, request.Id, slot, code)
	}
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		pipe.Del(ctx, key)
		pipe.HSet(ctx, key, values)
		pipe.PExpireAt(ctx, key, request.ExpireTime)
		_, err := pipe.Exec(ctx)
		server.Raise(err)
	})
}

// A waiting request as stored: its id, expiry, confirmed slots and fields.
type accountCodeState struct {
	Id         server.Id
	ExpireTime time.Time
	Confirmed  []bool
	Fields     map[string]string
}

// The owner's waiting request of the purpose, or nil.
func getAccountCodeRequest(ctx context.Context, purpose string, userId server.Id, slots int) *accountCodeState {
	var values map[string]string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		values, err = r.HGetAll(ctx, accountCodeKey(purpose, userId)).Result()
		if errors.Is(err, redis.Nil) {
			values = nil
			return
		}
		server.Raise(err)
	})
	if len(values) == 0 {
		return nil
	}
	requestId, err := server.ParseId(values[accountCodeIdField])
	if err != nil {
		return nil
	}
	expiresMillis, err := strconv.ParseInt(values[accountCodeExpiresField], 10, 64)
	if err != nil {
		return nil
	}
	state := &accountCodeState{
		Id:         requestId,
		ExpireTime: time.UnixMilli(expiresMillis).UTC(),
		Confirmed:  make([]bool, slots),
		Fields:     values,
	}
	for slot := range state.Confirmed {
		state.Confirmed[slot] = values[accountCodeConfirmedFieldPrefix+strconv.Itoa(slot)] == "1"
	}
	return state
}

// One try: KEYS[1] the request, ARGV[1] its id, ARGV[2] the try limit, then
// one candidate hmac per slot ("" for none). Returns {0} when the request is
// gone or replaced; {2} when this try reached the limit and deleted it; else
// {1, wrong, confirmed of each slot}.
var accountCodeTryScript = redis.NewScript(`
local key = KEYS[1]
if redis.call('HGET', key, 'id') ~= ARGV[1] then
    return {0}
end
local limit = tonumber(ARGV[2])
local slots = #ARGV - 2
local wrong = 0
for i = 1, slots do
    local candidate = ARGV[2 + i]
    if candidate ~= '' then
        local mac = redis.call('HGET', key, 'mac.' .. (i - 1))
        if mac and mac == candidate then
            redis.call('HSET', key, 'ok.' .. (i - 1), '1')
        else
            wrong = 1
        end
    end
end
if wrong == 1 then
    local tries = redis.call('HINCRBY', key, 'tries', 1)
    if limit <= tries then
        redis.call('DEL', key)
        return {2}
    end
end
local result = {1, wrong}
for i = 1, slots do
    if redis.call('HGET', key, 'ok.' .. (i - 1)) == '1' then
        table.insert(result, 1)
    else
        table.insert(result, 0)
    end
end
return result
`)

// What a try found.
type accountCodeTryResult struct {
	// the request is gone: it expired, was replaced, or was never made
	Missing bool
	// this try took the last of the request's tries and ended it
	Spent bool
	// a candidate was wrong (the right ones are confirmed all the same)
	Wrong bool
	// each slot's confirmation after the try
	Confirmed []bool
}

// Tries the candidate codes ("" for a slot not tried) against the request
// with the id. At most tryLimit tries with a wrong code are taken in all.
func tryAccountCodeRequest(ctx context.Context, purpose string, userId server.Id, requestId server.Id, candidates []string, tryLimit int) *accountCodeTryResult {
	args := []any{requestId.String(), tryLimit}
	for slot, candidate := range candidates {
		if candidate == "" {
			args = append(args, "")
		} else {
			args = append(args, accountCodeMac(purpose, userId, requestId, slot, candidate))
		}
	}
	var values []int64
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		values, err = accountCodeTryScript.Run(ctx, r, []string{accountCodeKey(purpose, userId)}, args...).Int64Slice()
		server.Raise(err)
	})
	switch {
	case len(values) == 0 || values[0] == 0:
		return &accountCodeTryResult{Missing: true}
	case values[0] == 2:
		return &accountCodeTryResult{Spent: true}
	case len(values) != 2+len(candidates):
		panic(fmt.Errorf("account code try answered %v", values))
	}
	result := &accountCodeTryResult{
		Wrong:     values[1] == 1,
		Confirmed: make([]bool, len(candidates)),
	}
	for slot := range result.Confirmed {
		result.Confirmed[slot] = values[2+slot] == 1
	}
	return result
}

// Deletes the request if it is still the one with the id. Returns whether it
// deleted it.
func removeAccountCodeRequest(ctx context.Context, purpose string, userId server.Id, requestId server.Id) (removed bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		count, err := accountCodeRemoveScript.Run(ctx, r, []string{accountCodeKey(purpose, userId)}, requestId.String()).Int64()
		server.Raise(err)
		removed = count == 1
	})
	return
}

var accountCodeRemoveScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'id') == ARGV[1] then
    return redis.call('DEL', KEYS[1])
end
return 0
`)
