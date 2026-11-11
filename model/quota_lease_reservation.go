package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// An edge lease's calls on one balance's reservations (DESIGN-DETAILS 13.2.2; QUOTA-M2-15), the
// operations quotahost's leases run (QUOTA-M2-07) on redisContractReservationScript with the balance's
// seven keys, through a QuotaRateRedis, so a host with no Postgres passes its ServiceHost.Redis and one
// with Postgres its own (NewQuotaRateServerRedis). Two kinds of token, both in the balance's
// {escrow_<balance_id>} hash:
//
//	lease/<lease_id>/<n>  draw n of a lease: bytes reserved and not yet carved, living a host's TokenTtl
//	                      (15 min) and extended while the lease lives; carve and settle only shrink it
//	svc/<usage_id>        bytes carved for one journal row: the debit's reservation until the flush
//	                      releases it (QD-087), living CarvedTokenTtl (24 h)
//
// Neither carries a pending or recovery marker, so the recovery page never enrolls one, and a token
// nobody releases returns at its lifetime with no debit (E-d). A reservation grants against the lower of
// the caller's reading of the balance and the balance's floor, the lowest reading shown within FloorTtl,
// which it lowers to that reading; both flushes lower it to the balance each debit left
// (release-debited), so no reading, however old, lets a reservation pass what the balance holds (L1).
//
// No call here touches Postgres (I-12). An error means Redis did not answer, or answered what this file
// cannot read: the caller does not know what the call did, and repeats it as it was, which every
// operation here answers idempotently.

type LeaseReservationSettings struct {
	// How long a balance's floor lives after it was last lowered: 2 min, at least twice the longest any
	// lease host uses a reading of a balance (its MaxCreditAge, 30 s), so the floor outlives every reading
	// taken before it was set (DESIGN-DETAILS 13.2.2's why the floor). Both flushes and every lease host
	// read this one default.
	FloorTtl time.Duration
	// How long a carved token lives unless its flush releases it: escrow's 24 h,
	// redisContractReservationLease, so a host killed between its carve and its post leaves the bytes
	// reserved for a day and debits nothing.
	CarvedTokenTtl time.Duration
}

func DefaultLeaseReservationSettings() *LeaseReservationSettings {
	return &LeaseReservationSettings{
		FloorTtl:       2 * time.Minute,
		CarvedTokenTtl: redisContractReservationLease,
	}
}

// The reservation script by the name a host's ServiceHost.Redis runs it under. The source is the one
// api's contract admission evaluates, so a lease and a contract judge one balance by one script.
var quotaLeaseReservationScript = &sidecar.RedisScript{
	Name:   "quota_lease_reservation",
	Source: redisContractReservationScript,
}

// Draw n of lease leaseId, n counting a lease's reservations from 1.
func QuotaLeaseToken(leaseId server.Id, n int) string {
	return fmt.Sprintf("lease/%s/%d", leaseId, n)
}

// The token a journal row's carved bytes are held under until the flush releases them.
func QuotaCarveToken(usageId server.Id) string {
	return "svc/" + usageId.String()
}

// A lease call's lease argument in whole milliseconds: at least 1, at most the script's day.
func quotaLeaseMilliseconds(ttl time.Duration) (string, error) {
	ms := ttl.Milliseconds()
	if ms < 1 || 24*60*60*1000 < ms {
		return "", fmt.Errorf("quota lease: a ttl of %s", ttl)
	}
	return strconv.FormatInt(ms, 10), nil
}

// One call of the reservation script on balanceId's seven keys, answering its reply.
func runQuotaLeaseReservation(ctx context.Context, redis QuotaRateRedis, balanceId server.Id, args []string) (*sidecar.RedisReply, error) {
	if redis == nil {
		return nil, errors.New("quota lease: no redis")
	}
	if balanceId == (server.Id{}) {
		return nil, errors.New("quota lease: a zero balance id")
	}
	return redis.RunScript(ctx, quotaLeaseReservationScript, redisLeaseReservationKeys(balanceId), args)
}

// A reply that is one amount: the script's decimal string, or its integer form.
func parseQuotaLeaseAmount(reply *sidecar.RedisReply) (ByteCount, error) {
	if reply == nil {
		return 0, errors.New("quota lease: no reply")
	}
	switch reply.Kind {
	case sidecar.RedisReplyBulk:
		amount, err := strconv.ParseInt(string(reply.Bulk), 10, 64)
		if err != nil || amount < 0 {
			return 0, fmt.Errorf("quota lease: an amount of %q", reply.Bulk)
		}
		return amount, nil
	case sidecar.RedisReplyInteger:
		if reply.Integer < 0 {
			return 0, fmt.Errorf("quota lease: an amount of %d", reply.Integer)
		}
		return reply.Integer, nil
	}
	return 0, fmt.Errorf("quota lease: a reply of kind %d", reply.Kind)
}

// Reserves up to requested of balanceId under token, granted against the lower of credit, the caller's
// reading of balance_byte_count (a negative balance reads 0), and the balance's floor, which the call
// lowers to credit when credit is lower or the floor is absent, for floorTtl. It answers what token
// holds: 0 when the balance had nothing to give. A retry under the same token answers the token's amount
// and reserves nothing more, so a host repeats an unanswered reservation as it was.
func ReserveLease(
	ctx context.Context,
	redis QuotaRateRedis,
	balanceId server.Id,
	token string,
	credit ByteCount,
	requested ByteCount,
	tokenTtl time.Duration,
	floorTtl time.Duration,
) (ByteCount, error) {
	if token == "" {
		return 0, errors.New("quota lease: no token")
	}
	if requested <= 0 {
		return 0, fmt.Errorf("quota lease: a reservation of %d", requested)
	}
	tokenMs, err := quotaLeaseMilliseconds(tokenTtl)
	if err != nil {
		return 0, err
	}
	floorMs, err := quotaLeaseMilliseconds(floorTtl)
	if err != nil {
		return 0, err
	}
	reply, err := runQuotaLeaseReservation(ctx, redis, balanceId, []string{
		"reserve",
		token,
		strconv.FormatInt(max(credit, 0), 10),
		strconv.FormatInt(requested, 10),
		tokenMs,
		floorMs,
	})
	if err != nil {
		return 0, err
	}
	amount, err := parseQuotaLeaseAmount(reply)
	if err != nil {
		return 0, err
	}
	if requested < amount {
		return 0, fmt.Errorf("quota lease: %d reserved of %d asked", amount, requested)
	}
	return amount, nil
}

// Sets token's expiry tokenTtl from now and answers what it holds: 0 when it is gone, lapsed or lost
// with Redis, which tells its host to stop spending it. An extend's answer is the token's amount, so it
// also settles what a carve whose answer never came did to it.
func ExtendLease(ctx context.Context, redis QuotaRateRedis, balanceId server.Id, token string, tokenTtl time.Duration) (ByteCount, error) {
	if token == "" {
		return 0, errors.New("quota lease: no token")
	}
	tokenMs, err := quotaLeaseMilliseconds(tokenTtl)
	if err != nil {
		return 0, err
	}
	reply, err := runQuotaLeaseReservation(ctx, redis, balanceId, []string{"extend", token, "0", "0", tokenMs})
	if err != nil {
		return 0, err
	}
	return parseQuotaLeaseAmount(reply)
}

// One source of a carve: amount of the lease token's bytes, which held exactly expect when the carve was
// first sent.
type LeaseCarvePart struct {
	Token           string
	ByteCount       ByteCount
	ExpectByteCount ByteCount
}

// Moves parts out of their lease tokens into svc/<usage_id> on balanceId, living carvedTtl, the sources
// that keep bytes living tokenTtl from now. moved: the destination holds byteCount, by this call or an
// earlier try of it, so the row may be posted. Not moved: a source was gone or no longer held its expect,
// so nothing moved, and the bytes are not to be billed. A carve that arrives again, however late, finds
// its destination or a source below its expect, and moves nothing twice (13.2.2's why expect).
func CarveLease(
	ctx context.Context,
	redis QuotaRateRedis,
	balanceId server.Id,
	usageId server.Id,
	parts []*LeaseCarvePart,
	carvedTtl time.Duration,
	tokenTtl time.Duration,
) (moved bool, byteCount ByteCount, returnErr error) {
	if usageId == (server.Id{}) {
		return false, 0, errors.New("quota lease: a zero usage id")
	}
	if len(parts) == 0 {
		return false, 0, errors.New("quota lease: a carve of nothing")
	}
	carvedMs, err := quotaLeaseMilliseconds(carvedTtl)
	if err != nil {
		return false, 0, err
	}
	tokenMs, err := quotaLeaseMilliseconds(tokenTtl)
	if err != nil {
		return false, 0, err
	}
	sum := ByteCount(0)
	partArgs := make([]string, 0, 3*len(parts))
	for _, part := range parts {
		if part == nil || part.Token == "" || part.ByteCount <= 0 || part.ExpectByteCount < part.ByteCount {
			return false, 0, fmt.Errorf("quota lease: a carve part of %+v", part)
		}
		sum += part.ByteCount
		if sum < 0 {
			return false, 0, errors.New("quota lease: a carve past int64")
		}
		partArgs = append(partArgs, part.Token, strconv.FormatInt(part.ByteCount, 10), strconv.FormatInt(part.ExpectByteCount, 10))
	}
	args := append([]string{
		"carve",
		QuotaCarveToken(usageId),
		"0",
		strconv.FormatInt(sum, 10),
		carvedMs,
		tokenMs,
	}, partArgs...)
	reply, err := runQuotaLeaseReservation(ctx, redis, balanceId, args)
	if err != nil {
		return false, 0, err
	}
	if reply == nil || reply.Kind != sidecar.RedisReplyArray || len(reply.Replies) != 2 {
		return false, 0, errors.New("quota lease: a carve answered no pair")
	}
	flag, err := parseQuotaLeaseAmount(reply.Replies[0])
	if err != nil {
		return false, 0, err
	}
	byteCount, err = parseQuotaLeaseAmount(reply.Replies[1])
	if err != nil {
		return false, 0, err
	}
	switch {
	case flag == 1 && byteCount == sum:
		return true, byteCount, nil
	case flag == 1:
		// the destination exists with other bytes: the usage id names another carve
		return false, 0, fmt.Errorf("quota lease: the carve's destination holds %d of %d", byteCount, sum)
	case flag == 0 && byteCount == 0:
		return false, 0, nil
	}
	return false, 0, fmt.Errorf("quota lease: a carve answered {%d, %d}", flag, byteCount)
}

// Shrinks token to keepByteCount, returning the rest to the balance: a lease's close, a draw keeping the
// bytes spent and not yet carved. It answers what the token holds, 0 when it is gone. Asking to keep more
// than the token holds is an error, with nothing changed.
func SettleLease(ctx context.Context, redis QuotaRateRedis, balanceId server.Id, token string, keepByteCount ByteCount, tokenTtl time.Duration) (ByteCount, error) {
	if token == "" {
		return 0, errors.New("quota lease: no token")
	}
	if keepByteCount < 0 {
		return 0, fmt.Errorf("quota lease: a settle keeping %d", keepByteCount)
	}
	tokenMs, err := quotaLeaseMilliseconds(tokenTtl)
	if err != nil {
		return 0, err
	}
	reply, err := runQuotaLeaseReservation(ctx, redis, balanceId, []string{"settle", token, "0", strconv.FormatInt(keepByteCount, 10), tokenMs})
	if err != nil {
		return 0, err
	}
	return parseQuotaLeaseAmount(reply)
}

// Releases token whole, returning its bytes to the balance: a lease's close for a draw with nothing
// spent left to carve. A token already gone releases nothing. A host never releases a carved token: its
// flush does, with the debit (release-debited).
func ReleaseLease(ctx context.Context, redis QuotaRateRedis, balanceId server.Id, token string, tokenTtl time.Duration) error {
	if token == "" {
		return errors.New("quota lease: no token")
	}
	tokenMs, err := quotaLeaseMilliseconds(tokenTtl)
	if err != nil {
		return err
	}
	reply, err := runQuotaLeaseReservation(ctx, redis, balanceId, []string{"release", token, "0", "0", tokenMs})
	if err != nil {
		return err
	}
	_, err = parseQuotaLeaseAmount(reply)
	return err
}
