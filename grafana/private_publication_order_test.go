// Exercises the real loader against a synthetic HTTP server: private content
// must never be uploaded while its previously public share remains reachable.
package grafana

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// All remote share identities and credentials are synthetic. Managed identities
// are read from the embedded source under test, never copied into fixtures.
type privatePublicationShare struct {
	Uid       string `json:"uid"`
	Token     string `json:"accessToken"`
	IsEnabled bool   `json:"isEnabled"`
}

// Records an explicit API sequence, not a timing-dependent exposure window.
type privatePublicationEvent struct {
	Operation string
	Uid       string
}

// Owns only this test's catalog, HTTP server and public-share state.
type privatePublicationFixture struct {
	server           *httptest.Server
	stateLock        sync.Mutex
	managedUidPublic map[string]bool
	shares           map[string]privatePublicationShare
	privateUid       string
	publicUid        string
	unmanagedUid     string
	readStatus       int
	malformedRead    bool
	missingIdentity  bool
	deleteStatus     int
	events           []privatePublicationEvent
	unsafeUploads    int
	unexpected       []string
}

// Reads the real managed set so adding a dashboard cannot evade the boundary.
func newPrivatePublicationFixture(t *testing.T) *privatePublicationFixture {
	t.Helper()
	fixture := &privatePublicationFixture{
		managedUidPublic: map[string]bool{},
		shares:           map[string]privatePublicationShare{},
		unmanagedUid:     "synthetic-unmanaged-dashboard",
	}
	entries, err := dashboardsFs.ReadDir("dashboards")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		content, err := dashboardsFs.ReadFile("dashboards/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		var dashboard struct {
			Uid  string   `json:"uid"`
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(content, &dashboard); err != nil || dashboard.Uid == "" {
			t.Fatal("embedded dashboard has invalid identity")
		}
		public := slices.Contains(dashboard.Tags, PublicTag)
		fixture.managedUidPublic[dashboard.Uid] = public
		if public && fixture.publicUid == "" {
			fixture.publicUid = dashboard.Uid
		}
		if !public && fixture.privateUid == "" {
			fixture.privateUid = dashboard.Uid
		}
	}
	if fixture.privateUid == "" || fixture.publicUid == "" || fixture.managedUidPublic[fixture.unmanagedUid] {
		t.Fatal("source catalog does not cover the private/public fixture boundary")
	}
	fixture.shares[fixture.unmanagedUid] = privatePublicationShare{
		Uid: "synthetic-unmanaged-share", Token: "synthetic-unmanaged-token", IsEnabled: true,
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(fixture.server.Close)
	return fixture
}

// State changes occur before the HTTP response is sent, giving exact ordering.
func (self *privatePublicationFixture) serve(response http.ResponseWriter, request *http.Request) {
	var upsert struct {
		Dashboard struct {
			Uid  string   `json:"uid"`
			Tags []string `json:"tags"`
		} `json:"dashboard"`
	}
	if request.Method == http.MethodPost && request.URL.Path == "/api/dashboards/db" {
		if err := json.NewDecoder(io.LimitReader(request.Body, 2*1024*1024)).Decode(&upsert); err != nil {
			http.Error(response, "synthetic invalid upsert", http.StatusBadRequest)
			return
		}
	}
	status, body := func() (int, any) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if request.Method == http.MethodGet && request.URL.Path == "/api/folders/"+FolderUid {
			return http.StatusOK, map[string]any{}
		}
		if request.Method == http.MethodPost && request.URL.Path == "/api/dashboards/db" {
			public, known := self.managedUidPublic[upsert.Dashboard.Uid]
			if !known || public != slices.Contains(upsert.Dashboard.Tags, PublicTag) {
				self.unexpected = append(self.unexpected, "upsert outside the managed source")
				return http.StatusBadRequest, map[string]any{}
			}
			self.events = append(self.events, privatePublicationEvent{Operation: "upsert", Uid: upsert.Dashboard.Uid})
			if share, exists := self.shares[upsert.Dashboard.Uid]; !public && exists && share.IsEnabled {
				self.unsafeUploads++
			}
			return http.StatusOK, map[string]any{"status": "success"}
		}
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(parts) >= 5 && parts[0] == "api" && parts[1] == "dashboards" && parts[2] == "uid" && parts[4] == "public-dashboards" {
			uid := parts[3]
			if _, known := self.managedUidPublic[uid]; !known {
				self.unexpected = append(self.unexpected, "share request outside managed source")
				return http.StatusBadRequest, map[string]any{}
			}
			share, exists := self.shares[uid]
			switch request.Method {
			case http.MethodGet:
				self.events = append(self.events, privatePublicationEvent{Operation: "read", Uid: uid})
				if uid == self.privateUid {
					if self.readStatus != 0 {
						return self.readStatus, map[string]any{"error": "synthetic share read failure"}
					}
					if self.malformedRead {
						return http.StatusOK, "synthetic nonobject share"
					}
					if self.missingIdentity {
						return http.StatusOK, map[string]any{"isEnabled": true}
					}
				}
				if !exists {
					return http.StatusNotFound, map[string]any{}
				}
				return http.StatusOK, share
			case http.MethodDelete:
				self.events = append(self.events, privatePublicationEvent{Operation: "delete", Uid: uid})
				if uid == self.privateUid && self.deleteStatus != 0 {
					return self.deleteStatus, map[string]any{"error": "synthetic unpublish failure"}
				}
				if !exists || len(parts) != 6 || parts[5] != share.Uid {
					return http.StatusNotFound, map[string]any{}
				}
				delete(self.shares, uid)
				return http.StatusOK, map[string]any{}
			case http.MethodPost:
				self.events = append(self.events, privatePublicationEvent{Operation: "publish", Uid: uid})
				share = privatePublicationShare{Uid: "synthetic-managed-share", Token: "synthetic-managed-token", IsEnabled: true}
				self.shares[uid] = share
				return http.StatusOK, share
			case http.MethodPatch:
				self.events = append(self.events, privatePublicationEvent{Operation: "enable", Uid: uid})
				share.IsEnabled = true
				self.shares[uid] = share
				return http.StatusOK, share
			}
		}
		self.unexpected = append(self.unexpected, request.Method+" "+request.URL.Path)
		return http.StatusNotFound, map[string]any{}
	}()
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

// Reads a completed sequence without racing the synthetic server handlers.
func (self *privatePublicationFixture) eventIndexes(uid, operation string) []int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	indexes := []int{}
	for index, event := range self.events {
		if event.Uid == uid && event.Operation == operation {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

// Only a synthetic admin talks to the local fake; no Grafana deployment runs.
func (self *privatePublicationFixture) load(t *testing.T) ([]string, []PublicDashboard, error) {
	t.Helper()
	return LoadDefaults(t.Context(), self.server.URL, "synthetic-admin", "synthetic-password")
}

// Sharing failure cannot be confused with harmless partial title progress.
func (self *privatePublicationFixture) requirePrivateNotUploaded(t *testing.T) {
	t.Helper()
	if len(self.eventIndexes(self.privateUid, "upsert")) != 0 {
		t.Fatal("private content was uploaded before its share could be safely removed")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.unsafeUploads != 0 || len(self.unexpected) != 0 {
		t.Fatalf("unsafe private uploads=%d; unexpected=%v", self.unsafeUploads, self.unexpected)
	}
}

// The pre-fix implementation deterministically uploads first and fails here.
func TestLoadDefaultsRevokesPublicShareBeforePrivateContentUpload(t *testing.T) {
	fixture := newPrivatePublicationFixture(t)
	fixture.shares[fixture.privateUid] = privatePublicationShare{Uid: "synthetic-private-share", Token: "synthetic-private-token", IsEnabled: true}
	if _, _, err := fixture.load(t); err != nil {
		t.Fatal(err)
	}
	deletes, uploads := fixture.eventIndexes(fixture.privateUid, "delete"), fixture.eventIndexes(fixture.privateUid, "upsert")
	if len(deletes) != 1 || len(uploads) != 1 || deletes[0] >= uploads[0] {
		t.Fatal("private dashboard share was not removed before content upload")
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if fixture.unsafeUploads != 0 || len(fixture.unexpected) != 0 {
		t.Fatalf("unsafe private uploads=%d; unexpected=%v", fixture.unsafeUploads, fixture.unexpected)
	}
}

// An unavailable sharing API cannot permit a new private content version.
func TestLoadDefaultsPrivateShareReadErrorPreventsContentUpload(t *testing.T) {
	fixture := newPrivatePublicationFixture(t)
	fixture.readStatus = http.StatusInternalServerError
	if _, _, err := fixture.load(t); err == nil {
		t.Fatal("share read failure was ignored")
	}
	fixture.requirePrivateNotUploaded(t)
}

// A failed DELETE leaves the old share live and must precede all private writes.
func TestLoadDefaultsPrivateShareDeleteErrorPreventsContentUpload(t *testing.T) {
	fixture := newPrivatePublicationFixture(t)
	fixture.shares[fixture.privateUid] = privatePublicationShare{Uid: "synthetic-private-share", Token: "synthetic-private-token", IsEnabled: true}
	fixture.deleteStatus = http.StatusInternalServerError
	if _, _, err := fixture.load(t); err == nil {
		t.Fatal("unpublish failure was ignored")
	}
	fixture.requirePrivateNotUploaded(t)
}

// Success status without valid share metadata is not absence evidence.
func TestLoadDefaultsPrivateShareMalformedReadPreventsContentUpload(t *testing.T) {
	fixture := newPrivatePublicationFixture(t)
	fixture.malformedRead = true
	if _, _, err := fixture.load(t); err == nil {
		t.Fatal("malformed share metadata was ignored")
	}
	fixture.requirePrivateNotUploaded(t)
}

// An unidentified existing share cannot be safely deleted by its dashboard uid.
func TestLoadDefaultsPrivateShareMissingIdentityPreventsContentUpload(t *testing.T) {
	fixture := newPrivatePublicationFixture(t)
	fixture.missingIdentity = true
	if _, _, err := fixture.load(t); err == nil {
		t.Fatal("missing share identity was ignored")
	}
	fixture.requirePrivateNotUploaded(t)
}

// A disabled share is still removed, rather than left available for re-enabling.
func TestLoadDefaultsPrivateDisabledShareIsRemovedBeforeContentUpload(t *testing.T) {
	fixture := newPrivatePublicationFixture(t)
	fixture.shares[fixture.privateUid] = privatePublicationShare{Uid: "synthetic-private-share", Token: "synthetic-private-token", IsEnabled: false}
	if _, _, err := fixture.load(t); err != nil {
		t.Fatal(err)
	}
	deletes, uploads := fixture.eventIndexes(fixture.privateUid, "delete"), fixture.eventIndexes(fixture.privateUid, "upsert")
	if len(deletes) != 1 || len(uploads) != 1 || deletes[0] >= uploads[0] {
		t.Fatal("disabled private share was retained across content upload")
	}
}

// A verified 404 permits the ordinary private upsert without a DELETE.
func TestLoadDefaultsPrivateUnsharedDashboardNeedsNoDelete(t *testing.T) {
	fixture := newPrivatePublicationFixture(t)
	if _, _, err := fixture.load(t); err != nil {
		t.Fatal(err)
	}
	if len(fixture.eventIndexes(fixture.privateUid, "delete")) != 0 || len(fixture.eventIndexes(fixture.privateUid, "upsert")) != 1 {
		t.Fatal("verified unshared dashboard did not load normally")
	}
}

// Repeating a successful load does not recreate or repeatedly delete privacy.
func TestLoadDefaultsPrivateShareRevocationIsIdempotent(t *testing.T) {
	fixture := newPrivatePublicationFixture(t)
	fixture.shares[fixture.privateUid] = privatePublicationShare{Uid: "synthetic-private-share", Token: "synthetic-private-token", IsEnabled: true}
	for range 2 {
		titles, _, err := fixture.load(t)
		if err != nil || len(titles) != len(fixture.managedUidPublic) {
			t.Fatalf("idempotent load failed: titles=%d error=%v", len(titles), err)
		}
	}
	if len(fixture.eventIndexes(fixture.privateUid, "delete")) != 1 || len(fixture.eventIndexes(fixture.privateUid, "upsert")) != 2 {
		t.Fatal("private sharing was not reconciled idempotently")
	}
}

// Tagged publication still follows upload; manually shared outsiders stay intact.
func TestLoadDefaultsPreservesPublicTagSharingAndUnmanagedShares(t *testing.T) {
	for _, state := range []string{"absent", "disabled", "enabled"} {
		fixture := newPrivatePublicationFixture(t)
		if state != "absent" {
			fixture.shares[fixture.publicUid] = privatePublicationShare{Uid: "synthetic-public-share", Token: "synthetic-public-token", IsEnabled: state == "enabled"}
		}
		_, public, err := fixture.load(t)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, dashboard := range public {
			found = found || dashboard.Uid == fixture.publicUid && dashboard.AccessToken != ""
		}
		if !found || len(fixture.eventIndexes(fixture.publicUid, "delete")) != 0 || len(fixture.eventIndexes(fixture.unmanagedUid, "read")) != 0 {
			t.Fatalf("public/managed scope changed for %s", state)
		}
		uploads := fixture.eventIndexes(fixture.publicUid, "upsert")
		operation := "publish"
		if state == "disabled" {
			operation = "enable"
		}
		changes := fixture.eventIndexes(fixture.publicUid, operation)
		if len(uploads) != 1 || state != "enabled" && (len(changes) != 1 || changes[0] <= uploads[0]) {
			t.Fatalf("tagged publication no longer follows content upload for %s", state)
		}
		fixture.stateLock.Lock()
		unmanaged := fixture.shares[fixture.unmanagedUid]
		unexpected := len(fixture.unexpected)
		fixture.stateLock.Unlock()
		if !unmanaged.IsEnabled || unmanaged.Uid != "synthetic-unmanaged-share" || unexpected != 0 {
			t.Fatalf("unmanaged sharing was changed for %s", state)
		}
	}
}
