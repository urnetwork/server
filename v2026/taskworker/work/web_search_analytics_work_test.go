package work

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Baidu's manual import is the one provider that runs without a network.
const testWebSearchAnalyticsBaiduConfig = `enabled: true
sites:
  - name: ur.io
    origin: https://ur.io
search:
  enabled: true
providers:
  baidu_search_resource_platform:
    enabled: true
    mode: manual_import
    interval: 1h
`

// Writes one manual import object under the prefix runBaidu lists.
func putWebSearchAnalyticsBaiduImport(t testing.TB, ctx context.Context, name string, body string) {
	store, ok := server.LoadBlobStore()
	if !ok {
		t.Fatal("test blob store is unavailable")
	}
	env, err := server.Env()
	if err != nil {
		env = "local"
	}
	localPath := filepath.Join(t.TempDir(), name)
	err = os.WriteFile(localPath, []byte(body), 0o600)
	server.Raise(err)
	err = store.Put(ctx, path.Join(store.Prefix(), env, "analytics/search/baidu", name), localPath, "text/csv")
	server.Raise(err)
}

// A first attempt that was interrupted or failed leaves a state row without
// last_success or cursor_time. Every later run read that row before its due
// check, so the task failed and was rescheduled without end. The next due run
// must ingest like a new stream, record its success and schedule a successor.
func TestWebSearchAnalyticsTaskRunsAfterFirstAttemptWithoutSuccess(t *testing.T) {
	for _, firstAttempt := range []string{"interrupted", "failed"} {
		t.Run(firstAttempt, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx := t.Context()
				t.Cleanup(server.Config.PushSimpleResource("analytics.yml", []byte(testWebSearchAnalyticsBaiduConfig)))
				t.Cleanup(server.Vault.PushSimpleResource("analytics.yml", []byte("{}\n")))
				server.UseLocalBlobStoreForTest(t, t.TempDir(), "blob", 1<<20)
				day := server.NowUtc().AddDate(0, 0, -3).Format("2006-01-02")
				putWebSearchAnalyticsBaiduImport(t, ctx, "first.csv", fmt.Sprintf(`site,date,query,clicks,impressions
ur.io,%s,ur network vpn,3,40
`, day))

				attemptTime := server.NowUtc().Add(-2 * time.Hour)
				model.MarkWebSearchAnalyticsIngestAttempt(ctx, "baidu", "*", "manual", attemptTime)
				if firstAttempt == "failed" {
					model.MarkWebSearchAnalyticsIngestError(ctx, "baidu", "*", "manual", attemptTime, "list_failed")
				}

				owner := session.NewLocalClientSession(ctx, "", nil)
				defer owner.Cancel()
				id := runProjectionAuditTask(t, owner, newAsyncMaintenanceCase(WebSearchAnalytics, WebSearchAnalyticsPost, &WebSearchAnalyticsArgs{}), true)
				var result WebSearchAnalyticsResult
				err := json.Unmarshal([]byte(task.GetFinishedTasks(ctx, id)[id].ResultJson), &result)
				server.Raise(err)
				if result.ProvidersAttempted != 1 || result.RowsAccepted != 1 || result.RowsRejected != 0 {
					t.Fatalf("run after a %s first attempt = %+v, want one provider and one accepted row", firstAttempt, result)
				}
				state := model.GetWebSearchAnalyticsIngestState(ctx, "baidu", "*", "manual")
				if !state.LastSuccess.After(attemptTime) || !state.LastAttempt.Equal(state.LastSuccess) || !state.CursorTime.Equal(state.LastSuccess) {
					t.Fatalf("state after a %s first attempt = %+v, want a success after %s", firstAttempt, state, attemptTime)
				}
			})
		})
	}
}
