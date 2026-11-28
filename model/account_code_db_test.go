package model

// The confirmation code requests (account_code.go) against the test env's
// Redis: a code confirms only its own slot of its own request, wrong tries are
// bounded even when they race, and a replaced or removed request confirms
// nothing.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A code of a request that is not code.
func accountCodeTestWrong(code string) string {
	if code == "000000" {
		return "111111"
	}
	return "000000"
}

func TestAccountCodeConfirmsOnlyItsOwnSlot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		request := newAccountCodeRequest("test", userId, 2, server.NowUtc().Add(time.Hour), map[string]string{"new_email": "new@example.com"})
		putAccountCodeRequest(ctx, request)

		state := getAccountCodeRequest(ctx, "test", userId, 2)
		if state == nil || state.Id != request.Id || state.Fields["new_email"] != "new@example.com" || state.Confirmed[0] || state.Confirmed[1] {
			t.Fatalf("state = %+v", state)
		}

		// slot 1's code in slot 0 is wrong
		if request.Codes[0] != request.Codes[1] {
			tried := tryAccountCodeRequest(ctx, "test", userId, request.Id, []string{request.Codes[1], ""}, 5)
			if tried.Missing || tried.Spent || !tried.Wrong || tried.Confirmed[0] || tried.Confirmed[1] {
				t.Fatalf("another slot's code: %+v", tried)
			}
		}
		// a code of this purpose's request never confirms another purpose's
		other := newAccountCodeRequest("other", userId, 2, server.NowUtc().Add(time.Hour), nil)
		putAccountCodeRequest(ctx, other)
		if other.Codes[0] != request.Codes[0] {
			if tried := tryAccountCodeRequest(ctx, "other", userId, other.Id, []string{request.Codes[0], ""}, 5); tried.Missing || !tried.Wrong || tried.Confirmed[0] {
				t.Fatalf("a code confirmed another purpose's request: %+v", tried)
			}
		}

		tried := tryAccountCodeRequest(ctx, "test", userId, request.Id, []string{request.Codes[0], ""}, 5)
		if tried.Missing || tried.Spent || tried.Wrong || !tried.Confirmed[0] || tried.Confirmed[1] {
			t.Fatalf("the current code: %+v", tried)
		}
		tried = tryAccountCodeRequest(ctx, "test", userId, request.Id, []string{"", request.Codes[1]}, 5)
		if tried.Wrong || !tried.Confirmed[0] || !tried.Confirmed[1] {
			t.Fatalf("the new code: %+v", tried)
		}
		state = getAccountCodeRequest(ctx, "test", userId, 2)
		if state == nil || !state.Confirmed[0] || !state.Confirmed[1] {
			t.Fatalf("state after both = %+v", state)
		}
		if !removeAccountCodeRequest(ctx, "test", userId, request.Id) {
			t.Fatal("the request was not removed")
		}
		if tried := tryAccountCodeRequest(ctx, "test", userId, request.Id, []string{request.Codes[0], ""}, 5); !tried.Missing {
			t.Fatalf("a removed request was tried: %+v", tried)
		}
	})
}

// A code made for one account's request does not confirm the same request id
// of another account, and a replaced request confirms nothing of the request
// it replaced.
func TestAccountCodeBindsTheAccountAndTheRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		otherUserId := server.NewId()
		first := newAccountCodeRequest("test", userId, 1, server.NowUtc().Add(time.Hour), nil)
		putAccountCodeRequest(ctx, first)
		// the other account's request, given the same id: its slot keeps the
		// hmac of its own code under its own account
		other := &accountCodeRequest{Purpose: "test", UserId: otherUserId, Id: first.Id, Codes: []string{accountCodeTestWrong(first.Codes[0])}, ExpireTime: first.ExpireTime}
		putAccountCodeRequest(ctx, other)
		if tried := tryAccountCodeRequest(ctx, "test", otherUserId, first.Id, []string{first.Codes[0]}, 5); tried.Missing || !tried.Wrong || tried.Confirmed[0] {
			t.Fatalf("one account's code confirmed another's request: %+v", tried)
		}

		second := newAccountCodeRequest("test", userId, 1, server.NowUtc().Add(time.Hour), nil)
		putAccountCodeRequest(ctx, second)
		if tried := tryAccountCodeRequest(ctx, "test", userId, first.Id, []string{first.Codes[0]}, 5); !tried.Missing {
			t.Fatalf("a replaced request was tried: %+v", tried)
		}
		if tried := tryAccountCodeRequest(ctx, "test", userId, second.Id, []string{first.Codes[0]}, 5); first.Codes[0] != second.Codes[0] && (!tried.Wrong || tried.Confirmed[0]) {
			t.Fatalf("the replaced request's code confirmed its replacement: %+v", tried)
		}
		if removeAccountCodeRequest(ctx, "test", userId, first.Id) {
			t.Fatal("removing a replaced request removed its replacement")
		}
		if getAccountCodeRequest(ctx, "test", userId, 1) == nil {
			t.Fatal("the replacement is gone")
		}
	})
}

// Concurrent wrong tries take the request's tries one by one: exactly the try
// limit of them is counted, the one that reaches it deletes the request, and
// the right code fails afterwards.
func TestAccountCodeWrongTriesAreBoundedWhenTheyRace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		request := newAccountCodeRequest("test", userId, 1, server.NowUtc().Add(time.Hour), nil)
		putAccountCodeRequest(ctx, request)
		wrong := accountCodeTestWrong(request.Codes[0])

		const tryLimit = 5
		results := make([]*accountCodeTryResult, 3*tryLimit)
		var wait sync.WaitGroup
		for i := range results {
			wait.Add(1)
			go func() {
				defer wait.Done()
				results[i] = tryAccountCodeRequest(ctx, "test", userId, request.Id, []string{wrong}, tryLimit)
			}()
		}
		wait.Wait()

		wrongCount, spentCount, missingCount := 0, 0, 0
		for _, result := range results {
			switch {
			case result.Spent:
				spentCount += 1
			case result.Missing:
				missingCount += 1
			case result.Wrong:
				wrongCount += 1
			}
		}
		if wrongCount != tryLimit-1 || spentCount != 1 || missingCount != len(results)-tryLimit {
			t.Fatalf("wrong %d, spent %d, missing %d of %d tries", wrongCount, spentCount, missingCount, len(results))
		}
		if tried := tryAccountCodeRequest(ctx, "test", userId, request.Id, []string{request.Codes[0]}, tryLimit); !tried.Missing {
			t.Fatalf("the right code worked after the tries were spent: %+v", tried)
		}
	})
}

// A request ends at its expiry: Redis drops it.
func TestAccountCodeRequestExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		request := newAccountCodeRequest("test", userId, 1, server.NowUtc().Add(-time.Second), nil)
		putAccountCodeRequest(ctx, request)
		if state := getAccountCodeRequest(ctx, "test", userId, 1); state != nil {
			t.Fatalf("an expired request is waiting: %+v", state)
		}
		if tried := tryAccountCodeRequest(ctx, "test", userId, request.Id, []string{request.Codes[0]}, 5); !tried.Missing {
			t.Fatalf("an expired request was tried: %+v", tried)
		}
	})
}

// The hmac binds every part, each delimited: the key, purpose, user,
// request, slot and code.
// A pure test: it needs no vault, so it keys the hmac itself.
func TestAccountCodeMacBindsEveryPart(t *testing.T) {
	key := []byte("a synthetic pepper for the account code test")
	userId := server.NewId()
	requestId := server.NewId()
	mac := func(key []byte, purpose string, userId server.Id, requestId server.Id, slot int, code string) string {
		return accountCodeMacWithKey(key, purpose, userId, requestId, slot, code)
	}
	base := mac(key, "test", userId, requestId, 0, "123456")
	for name, other := range map[string]string{
		"key":     mac([]byte("another pepper"), "test", userId, requestId, 0, "123456"),
		"purpose": mac(key, "other", userId, requestId, 0, "123456"),
		"user":    mac(key, "test", server.NewId(), requestId, 0, "123456"),
		"request": mac(key, "test", userId, server.NewId(), 0, "123456"),
		"slot":    mac(key, "test", userId, requestId, 1, "123456"),
		"code":    mac(key, "test", userId, requestId, 0, "123457"),
		// the parts are delimited, so moving a character between two parts
		// is another hmac
		"boundary": mac(key, "test1", userId, requestId, 0, "23456"),
	} {
		if other == base {
			t.Errorf("another %s gives the same hmac", name)
		}
	}
	if mac(key, "test", userId, requestId, 0, "123456") != base {
		t.Fatal("the hmac is not deterministic")
	}
}
