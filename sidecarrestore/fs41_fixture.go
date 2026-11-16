package sidecarrestore

// FS-41's frame (D-OPS-14, R-OPS-9, FS-41, FI-5, FI-10, FI-12; OPS-M3-05): a site loss, on which §19.10's site steps
// run in order, this task's close and the owners' steps on it, EVSB-M2-20's re-seeds, TEAM-M3-02's resumed log and
// KEYS-M2-25's re-formed groups, each at its milestone (DNS-M7-02, MAIL's M7 tasks, SDK-M8-12 and SMTP-M9-19 add theirs).
//
// The site's store, the file clusters, is a sidecartest.FakeBlobs the test seeds before the loss. LoseSite then loses
// everything at once: the primary, which comes back from both dumps through xops's real scripts as PostgresLossFixture
// restores it, under a site incident; and every bucket, so the store a step sees is a new, empty one, which the
// hardware step finds empty and answering. reseed: true goes into sidecar.yml, which limits evs and gkm to re-seed
// uploads and GroupForms and opens api's log upload (§19.10's hardware row), until EndReseed takes it off for the
// close. What the devices bring back, the test writes on the new store through the fixture: a log resumed verbatim
// (ResumeLog, what TEAM-M3-02's adoption leaves at unchanged positions) and a repo re-seeded, placed afresh with its
// repo/ and, where it came back below a surviving reader's pin, a Signed EvsReseed under reseed/ (ReseedRepo, what
// EVSB-M2-20's /reseed leaves).
//
// The new store is a fake, not T3's MinIO clusters: drives lost past parity, the real wipe and §22.9's suite on the
// new hardware are EVSB-M2-20's file_cluster_reseed_test.go on T3, which this fixture does not run.

import (
	"context"
	"fmt"
	"io"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// One site loss: the Postgres loss under it, the site's store and the new hardware's.
type SiteLossFixture struct {
	*PostgresLossFixture
	siteBlobs *sidecartest.FakeBlobs
	newBlobs  *sidecartest.FakeBlobs
	// sidecar.yml's lines beside reseed, which the test keeps, such as its preview networks
	sidecarYaml string
	popConfig   func()
}

// A fixture over the env's private database and siteBlobs, the site's store, which the test seeds before the loss;
// settings nil for the defaults. sidecarYaml holds sidecar.yml's lines under its sidecar: key that the test keeps
// through the loss, each indented two spaces. Close it as PostgresLossFixture is closed.
func NewSiteLossFixture(t PostgresLossT, settings *PostgresLossSettings, siteBlobs *sidecartest.FakeBlobs, sidecarYaml string) *SiteLossFixture {
	t.Helper()
	return &SiteLossFixture{
		PostgresLossFixture: NewPostgresLossFixture(t, settings),
		siteBlobs:           siteBlobs,
		sidecarYaml:         sidecarYaml,
	}
}

// Ends what the fixture started, the Postgres loss's and sidecar.yml's push.
func (self *SiteLossFixture) Close() {
	if self.popConfig != nil {
		self.popConfig()
		self.popConfig = nil
	}
	self.PostgresLossFixture.Close()
}

// The store the site held, which the test seeds before LoseSite.
func (self *SiteLossFixture) SiteBlobs() *sidecartest.FakeBlobs {
	return self.siteBlobs
}

// The new hardware's store, empty at LoseSite; nil before it.
func (self *SiteLossFixture) NewBlobs() *sidecartest.FakeBlobs {
	return self.newBlobs
}

// Sets sidecar.yml's reseed, with the test's lines beside it.
func (self *SiteLossFixture) setReseed(on bool) {
	if self.popConfig != nil {
		self.popConfig()
	}
	self.popConfig = server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  reseed: %t\n%s", on, self.sidecarYaml))
}

// Loses the site under incidentId (see the file header): both dumps restored under a site incident, every bucket
// gone, and reseed on. Answers the incident.
func (self *SiteLossFixture) LoseSite(incidentId server.Id) *Incident {
	self.t.Helper()
	incident := self.restoreDumps(incidentId, IncidentKindSite)
	settings := sidecartest.DefaultFakeBlobsSettings()
	self.newBlobs = sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, settings)
	self.setReseed(true)
	return incident
}

// Takes reseed off, as §19.10's close asks after its 30 days.
func (self *SiteLossFixture) EndReseed() {
	self.setReseed(false)
}

// Whether every cluster of the new store is empty and answers a LIST: the hardware row's empty buckets.
func (self *SiteLossFixture) NewStoreEmpty(ctx context.Context) error {
	if self.newBlobs == nil {
		return fmt.Errorf("the site is not lost yet")
	}
	clusterIds := append(self.newBlobs.MetadataClusterIds(), sidecartest.DefaultFakeBlobsSettings().SegmentClusterId)
	for _, clusterId := range clusterIds {
		cluster, err := self.newBlobs.Cluster(clusterId)
		if err != nil {
			return err
		}
		infos, _, err := cluster.List(ctx, "", "", 1)
		if err != nil {
			return fmt.Errorf("cluster %s does not answer: %w", clusterId, err)
		}
		if 0 < len(infos) {
			return fmt.Errorf("cluster %s is not empty", clusterId)
		}
	}
	return nil
}

// Copies networkId's log, every proj/ and projrows/ object of it, its entries and its bases' rows, from the site's
// store to the new first cluster, verbatim: the log resumed at unchanged positions, as TEAM-M3-02's adoption of
// members' uploaded entries leaves it.
func (self *SiteLossFixture) ResumeLog(ctx context.Context, networkId server.Id) {
	self.t.Helper()
	for _, family := range []string{"proj/", "projrows/"} {
		self.copyPrefix(ctx, family+networkId.String()+"/")
	}
}

// Copies every object under prefix from the site's first cluster to the new one, each written once.
func (self *SiteLossFixture) copyPrefix(ctx context.Context, prefix string) {
	self.t.Helper()
	from, err := self.siteBlobs.FirstCluster()
	if err != nil {
		self.t.Fatalf("%v", err)
	}
	to, err := self.newBlobs.FirstCluster()
	if err != nil {
		self.t.Fatalf("%v", err)
	}
	startAfter := ""
	for {
		infos, more, err := from.List(ctx, prefix, startAfter, 1000)
		if err != nil {
			self.t.Fatalf("%v", err)
		}
		for _, info := range infos {
			object, err := from.Get(ctx, info.Key)
			if err != nil {
				self.t.Fatalf("%v", err)
			}
			body, err := io.ReadAll(object.Body)
			object.Body.Close()
			if err != nil {
				self.t.Fatalf("%v", err)
			}
			self.putOnce(ctx, to, info.Key, body)
			startAfter = info.Key
		}
		if !more || len(infos) == 0 {
			return
		}
	}
}

// Re-seeds repoId of networkId on the new store, as a re-seed's first writes leave it: placed afresh on the first
// cluster with its repo/ active there and, with underStatement, a Signed EvsReseed under reseed/<repo>/<ref>, where it
// came back below a surviving reader's pin.
func (self *SiteLossFixture) ReseedRepo(ctx context.Context, networkId server.Id, repoId sidecar.Id, kind protocol.RepoKind, underStatement bool) {
	self.t.Helper()
	first, err := self.newBlobs.FirstCluster()
	if err != nil {
		self.t.Fatalf("%v", err)
	}
	placement, err := (&sidecar.EvsRepoPlace{ClusterId: first.ClusterId(), CreatorSidecarId: sidecar.NewId()}).Marshal()
	if err != nil {
		self.t.Fatalf("%v", err)
	}
	self.putOnce(ctx, first, sidecar.EvsPlaceKey(repoId), placement)
	object, err := sidecar.MarshalEvsRepoObject(&protocol.EvsRepoObject{
		RepoId:    repoId.Bytes(),
		Kind:      kind,
		NetworkId: sidecar.Id(networkId).Bytes(),
		State:     protocol.EvsRepoState_EVS_REPO_ACTIVE,
		Format:    sidecar.EvsRepoFormat1,
	})
	if err != nil {
		self.t.Fatalf("%v", err)
	}
	self.putOnce(ctx, first, sidecar.EvsRepoObjectKey(repoId), object)
	if underStatement {
		// one ref's statement, at a key fixed by the repo, so a step that runs again writes nothing more
		self.putOnce(ctx, first, "reseed/"+repoId.String()+"/"+repoId.String(), []byte("a Signed EvsReseed"))
	}
}

// Writes key once: a step the frame runs again finds it written and writes nothing.
func (self *SiteLossFixture) putOnce(ctx context.Context, cluster sidecar.BlobCluster, key string, body []byte) {
	self.t.Helper()
	if _, err := cluster.Head(ctx, key); err == nil {
		return
	}
	if _, err := cluster.PutIfNoneMatch(ctx, key, body, nil); err != nil {
		self.t.Fatalf("%v", err)
	}
}
