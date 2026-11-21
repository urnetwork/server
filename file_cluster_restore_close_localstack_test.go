//go:build sidecar_localstack && sidecar_pending

package server_test

// Runs the registered site-loss close with the operator's real restricted MinIO users and a private
// restored database. A repo can be on either metadata cluster or missing from the replacement store.
import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarrestore"
)

// A real, signed config projection; the fixture writer supplies the placement, repo and optional EVS marker.
func seedCloseProjection(t testing.TB, writer *server.FileClusters, placement string, underReseed bool) sidecar.Id {
	t.Helper()
	ctx := t.Context()
	first, err := writer.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	world := sidecartest.NewConfigLogSuiteWorld(ctx, first)
	settings := model.DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := model.NewConfigLogSequencer(first, settings, nil)
	header, rows, genesis, pushes := world.BaseOne()
	networkId := server.Id(world.NetworkId())
	_, err = sequencer.AppendBase(ctx, networkId, &model.ConfigLogBase{
		SignedHeader: header, Rows: rows, Genesis: genesis, ConfigPushes: pushes,
	})
	if err != nil {
		t.Fatal(err)
	}
	repos, found, err := sequencer.RestoreRepoRows(ctx, networkId)
	if err != nil || !found || len(repos) != 1 {
		t.Fatalf("restored projection: %d repos, found %t, %v", len(repos), found, err)
	}
	repoId, err := sidecar.IdFromBytes(repos[0].GetRepoId())
	if err != nil {
		t.Fatal(err)
	}
	if placement == "" {
		return repoId
	}
	cluster, err := writer.Cluster(placement)
	if err != nil {
		t.Fatal(err)
	}
	value, err := (&sidecar.EvsRepoPlace{ClusterId: placement, CreatorSidecarId: sidecar.NewId()}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutIfNoneMatch(ctx, sidecar.EvsPlaceKey(repoId), value, nil); err != nil {
		t.Fatal(err)
	}
	value, err = sidecar.MarshalEvsRepoObject(&protocol.EvsRepoObject{
		RepoId: repoId.Bytes(), Kind: repos[0].GetKind(), NetworkId: world.NetworkId().Bytes(),
		State: protocol.EvsRepoState_EVS_REPO_ACTIVE, Format: sidecar.EvsRepoFormat1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cluster.PutIfNoneMatch(ctx, sidecar.EvsRepoObjectKey(repoId), value, nil); err != nil {
		t.Fatal(err)
	}
	if underReseed {
		if _, err := cluster.PutIfNoneMatch(ctx, evsservice.ReseedKey(repoId, sidecar.NewId()), []byte("synthetic statement"), nil); err != nil {
			t.Fatal(err)
		}
	}
	return repoId
}

func TestSiteCloseWithRestrictedUsersAcrossMetadataClusters(t *testing.T) {
	writer := server.TestingRestorePolicyClusters(t)
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(test testing.TB) {
		ctx := test.Context()
		seedCloseProjection(test, writer, "meta-1", false)
		under := seedCloseProjection(test, writer, "meta-2", true)
		missing := seedCloseProjection(test, writer, "", false)

		id := server.NewId()
		listPath := filepath.Join(test.TempDir(), "incident.tsv")
		at := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
		list := fmt.Sprintf("snapshot-time\tsidecar dump\t%s\t%s\nsnapshot-time\tmain dump\t%s\t%s\n", id, at, id, at)
		if err := os.WriteFile(listPath, []byte(list), 0600); err != nil {
			test.Fatal(err)
		}
		incident, err := sidecarrestore.OpenIncident(id, listPath, nil)
		if err != nil {
			test.Fatal(err)
		}
		if err := incident.SetKind(sidecarrestore.IncidentKindSite); err != nil {
			test.Fatal(err)
		}
		frame := sidecarrestore.DefaultFrame()
		for _, step := range frame.Steps() {
			if step.Name == sidecarrestore.StepClose {
				break
			}
			if err := incident.Append(&sidecarrestore.ListRow{Kind: sidecarrestore.ListKindStepEnded, Table: step.Name, RowId: id.String(), Detail: at + " ran"}); err != nil {
				test.Fatal(err)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
				"DO $$ BEGIN EXECUTE format('COMMENT ON DATABASE %%I IS %%L', current_database(), '%s'); END $$", sidecarrestore.RestoreMarker(id))))
		})
		defer server.Testing_PushSidecarConfig("sidecar:\n  reseed: false\n")()
		// The public API's credential is unavailable to this operator.
		defer server.Vault.PushSimpleResource("file-clusters-api.yml", []byte("clusters: {}\n"))()
		if err := frame.RunStep(ctx, incident, sidecarrestore.StepClose, io.Discard, ""); err != nil {
			test.Fatal(err)
		}
		for kind, repo := range map[string]sidecar.Id{
			sidecarrestore.ListKindCloseNotReseeded: missing,
			sidecarrestore.ListKindCloseUnderReseed: under,
		} {
			rows := incident.RowsOfKind(kind)
			if len(rows) != 1 || rows[0].RowId != repo.String() {
				test.Fatalf("%s records %v, want repo %s", kind, rows, repo)
			}
		}
	})
}
