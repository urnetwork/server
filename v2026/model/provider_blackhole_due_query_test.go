// Query-boundary controls are deterministic and run without external state.
package model

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type testBlackholeDueQuery func(context.Context, string, ...any) (server.PgResult, error)

func (self testBlackholeDueQuery) Query(ctx context.Context, sql string, args ...any) (server.PgResult, error) {
	return self(ctx, sql, args...)
}

type testBlackholeDueRows struct {
	server.PgResult
	ids     []server.Id
	index   int
	onClose func()
}

func (self *testBlackholeDueRows) Next() bool { self.index++; return self.index <= len(self.ids) }
func (self *testBlackholeDueRows) Scan(values ...any) error {
	*values[0].(*server.Id) = self.ids[self.index-1]
	return nil
}
func (self *testBlackholeDueRows) Err() error { return nil }
func (self *testBlackholeDueRows) Close() {
	if self.onClose != nil {
		self.onClose()
	}
}

func TestBlackholeDueReadsOnlyTwoBoundedHeads(t *testing.T) {
	for _, limit := range []int{1, 2, 3, 8, 5000} {
		calls := 0
		query := testBlackholeDueQuery(func(_ context.Context, sql string, args ...any) (server.PgResult, error) {
			calls++
			if calls > 2 {
				t.Fatal("due query performed unbounded head scanning")
			}
			limitIndex := 2
			if calls == 2 {
				limitIndex = 1
			}
			if args[limitIndex] != limit || !strings.Contains(sql, "LIMIT $") {
				t.Fatalf("head widened limit=%d args=%v", limit, args)
			}
			ids := make([]server.Id, limit)
			for index := range ids {
				ids[index] = server.Id{byte(calls), byte(index / 256), byte(index % 256)}
			}
			return &testBlackholeDueRows{ids: ids}, nil
		})
		ids := getProviderBlackholeCheckDueWithQuery(context.Background(), query, time.Unix(1000, 0), limit, 2, 4)
		wantCalls := 2
		if limit == 1 {
			wantCalls = 1
		}
		if calls != wantCalls || len(ids) != limit {
			t.Fatalf("limit=%d calls=%d count=%d", limit, calls, len(ids))
		}
		for index, id := range ids {
			if id[0] != byte(1+index%2) {
				t.Fatalf("limit=%d index=%d lost fair prefix", limit, index)
			}
		}
	}
}

func TestBlackholeDueDeduplicatesCategoryMovement(t *testing.T) {
	a, b, c := server.Id{1}, server.Id{2}, server.Id{3}
	calls := 0
	query := testBlackholeDueQuery(func(context.Context, string, ...any) (server.PgResult, error) {
		calls++
		if calls == 1 {
			return &testBlackholeDueRows{ids: []server.Id{a, b}}, nil
		}
		return &testBlackholeDueRows{ids: []server.Id{a, c}}, nil
	})
	got := getProviderBlackholeCheckDueWithQuery(context.Background(), query, time.Unix(1000, 0), 3, 0, 1)
	if !slices.Equal(got, []server.Id{a, c, b}) || calls != 2 {
		t.Fatalf("moving category duplicated/stranded candidate: %v calls=%d", got, calls)
	}
}

func TestBlackholeDueLookupFailureDoesNotReturnPartialHealthy(t *testing.T) {
	want := errors.New("synthetic first-head lookup failure")
	calls := 0
	query := testBlackholeDueQuery(func(context.Context, string, ...any) (server.PgResult, error) {
		calls++
		if calls == 1 {
			return &testBlackholeDueRows{ids: []server.Id{{1}}}, nil
		}
		return nil, want
	})
	err := server.HandleError(func() {
		getProviderBlackholeCheckDueWithQuery(context.Background(), query, time.Unix(1000, 0), 2, 0, 1)
	})
	if err != want || calls != 2 {
		t.Fatalf("head error became a partial healthy queue: error=%v calls=%d", err, calls)
	}
}

func TestBlackholeDueCancellationStopsSecondHead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	query := testBlackholeDueQuery(func(context.Context, string, ...any) (server.PgResult, error) {
		calls++
		return &testBlackholeDueRows{ids: []server.Id{{1}}, onClose: cancel}, nil
	})
	err := server.HandleError(func() { getProviderBlackholeCheckDueWithQuery(ctx, query, time.Unix(1000, 0), 2, 0, 1) })
	if err != context.Canceled || calls != 1 {
		t.Fatalf("canceled selector continued: error=%v calls=%d", err, calls)
	}
}
