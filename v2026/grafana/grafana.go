package grafana

// default grafana dashboards for the urnetwork services,
// loaded into the grafana service with `bringyourctl grafana load-defaults`.
// the dashboards reference the provisioned datasource uids warp-loki and
// warp-mimir (see warp/grafana in the warp repo), and have stable uids so
// that loading is an idempotent upsert into the urnetwork folder.
//
// a dashboard tagged "public" (in its json tags) is additionally published as
// a grafana public dashboard (read-only, no login) at
// <grafanaUrl>/public-dashboards/<accessToken>. the tag is the source of
// truth: LoadDefaults publishes tagged dashboards and unpublishes managed
// dashboards whose tag was removed. note grafana public dashboards do not
// support template variables, so public dashboards must use fixed queries.
// the warp go front serves a directory of the public dashboards at
// <env>-grafana.<domain>/stats (see warp/grafana/main.go)

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

//go:embed dashboards/*.json
var dashboardsFs embed.FS

const FolderUid = "urnetwork"
const FolderTitle = "urnetwork"

// the dashboard tag that marks a dashboard for public (read-only, no login)
// sharing
const PublicTag = "public"

// a published public dashboard, returned by LoadDefaults.
// the public view url is <grafanaUrl>/public-dashboards/<AccessToken>
type PublicDashboard struct {
	Title       string
	Uid         string
	AccessToken string
}

// LoadDefaults upserts the default dashboards into the grafana
// at grafanaUrl, in the urnetwork folder, and reconciles public dashboard
// sharing from the "public" tag.
// returns the loaded dashboard titles and the published public dashboards
func LoadDefaults(ctx context.Context, grafanaUrl string, username string, password string) ([]string, []PublicDashboard, error) {
	grafanaUrl = strings.TrimSuffix(grafanaUrl, "/")

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	do := func(method string, path string, requestBody any) (int, []byte, error) {
		var bodyReader io.Reader
		if requestBody != nil {
			bodyJson, err := json.Marshal(requestBody)
			if err != nil {
				return 0, nil, err
			}
			bodyReader = bytes.NewReader(bodyJson)
		}
		request, err := http.NewRequestWithContext(ctx, method, grafanaUrl+path, bodyReader)
		if err != nil {
			return 0, nil, err
		}
		request.SetBasicAuth(username, password)
		if requestBody != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return 0, nil, err
		}
		defer response.Body.Close()
		responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
		if err != nil {
			return 0, nil, err
		}
		return response.StatusCode, responseBody, nil
	}

	// ensure the folder
	statusCode, _, err := do("GET", fmt.Sprintf("/api/folders/%s", FolderUid), nil)
	if err != nil {
		return nil, nil, err
	}
	if statusCode == 404 {
		statusCode, responseBody, err := do("POST", "/api/folders", map[string]any{
			"uid":   FolderUid,
			"title": FolderTitle,
		})
		if err != nil {
			return nil, nil, err
		}
		if 400 <= statusCode {
			return nil, nil, errors.New(fmt.Sprintf("Could not create folder (%d): %s", statusCode, string(responseBody)))
		}
	} else if 400 <= statusCode {
		return nil, nil, errors.New(fmt.Sprintf("Could not read folder (%d).", statusCode))
	}

	entries, err := dashboardsFs.ReadDir("dashboards")
	if err != nil {
		return nil, nil, err
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)

	// a managed dashboard and whether it is tagged public
	type managedDashboard struct {
		uid    string
		title  string
		public bool
	}

	titles := []string{}
	managed := []managedDashboard{}
	for _, name := range names {
		dashboardJson, err := dashboardsFs.ReadFile(fmt.Sprintf("dashboards/%s", name))
		if err != nil {
			return nil, nil, err
		}
		dashboard := map[string]any{}
		if err := json.Unmarshal(dashboardJson, &dashboard); err != nil {
			return nil, nil, errors.New(fmt.Sprintf("Could not parse %s (%s)", name, err))
		}
		// the id is per grafana instance. upsert by uid
		delete(dashboard, "id")

		statusCode, responseBody, err := do("POST", "/api/dashboards/db", map[string]any{
			"dashboard": dashboard,
			"folderUid": FolderUid,
			"overwrite": true,
			"message":   "bringyourctl grafana load-defaults",
		})
		if err != nil {
			return nil, nil, err
		}
		if 400 <= statusCode {
			return nil, nil, errors.New(fmt.Sprintf("Could not load %s (%d): %s", name, statusCode, string(responseBody)))
		}

		title, _ := dashboard["title"].(string)
		titles = append(titles, title)

		uid, _ := dashboard["uid"].(string)
		managed = append(managed, managedDashboard{
			uid:    uid,
			title:  title,
			public: hasTag(dashboard, PublicTag),
		})
	}

	// reconcile public dashboard sharing. the "public" tag is the source of
	// truth: publish tagged dashboards, unpublish managed dashboards no longer
	// tagged. dashboards outside this managed set are never touched
	public := []PublicDashboard{}
	for _, m := range managed {
		if m.uid == "" {
			continue
		}
		publicPath := fmt.Sprintf("/api/dashboards/uid/%s/public-dashboards/", m.uid)

		getStatus, getBody, err := do("GET", publicPath, nil)
		if err != nil {
			return nil, nil, err
		}
		exists := getStatus == 200
		if !exists && getStatus != 404 {
			return nil, nil, errors.New(fmt.Sprintf("Could not read public dashboard for %s (%d): %s", m.uid, getStatus, string(getBody)))
		}
		var current struct {
			Uid         string `json:"uid"`
			AccessToken string `json:"accessToken"`
			IsEnabled   bool   `json:"isEnabled"`
		}
		if exists {
			if err := json.Unmarshal(getBody, &current); err != nil {
				return nil, nil, err
			}
		}

		switch {
		case m.public && !exists:
			// publish
			postStatus, postBody, err := do("POST", publicPath, map[string]any{
				"isEnabled":            true,
				"share":                "public",
				"timeSelectionEnabled": true,
			})
			if err != nil {
				return nil, nil, err
			}
			if 400 <= postStatus {
				return nil, nil, errors.New(fmt.Sprintf("Could not publish %s (%d): %s", m.uid, postStatus, string(postBody)))
			}
			if err := json.Unmarshal(postBody, &current); err != nil {
				return nil, nil, err
			}
			public = append(public, PublicDashboard{Title: m.title, Uid: m.uid, AccessToken: current.AccessToken})
		case m.public && exists:
			// ensure enabled
			if !current.IsEnabled {
				patchPath := fmt.Sprintf("/api/dashboards/uid/%s/public-dashboards/%s", m.uid, current.Uid)
				patchStatus, patchBody, err := do("PATCH", patchPath, map[string]any{
					"isEnabled":            true,
					"share":                "public",
					"timeSelectionEnabled": true,
				})
				if err != nil {
					return nil, nil, err
				}
				if 400 <= patchStatus {
					return nil, nil, errors.New(fmt.Sprintf("Could not enable public %s (%d): %s", m.uid, patchStatus, string(patchBody)))
				}
			}
			public = append(public, PublicDashboard{Title: m.title, Uid: m.uid, AccessToken: current.AccessToken})
		case !m.public && exists:
			// unpublish (tag removed)
			deletePath := fmt.Sprintf("/api/dashboards/uid/%s/public-dashboards/%s", m.uid, current.Uid)
			deleteStatus, deleteBody, err := do("DELETE", deletePath, nil)
			if err != nil {
				return nil, nil, err
			}
			if 400 <= deleteStatus {
				return nil, nil, errors.New(fmt.Sprintf("Could not unpublish %s (%d): %s", m.uid, deleteStatus, string(deleteBody)))
			}
		}
	}

	return titles, public, nil
}

// hasTag reports whether the dashboard json has the given tag
func hasTag(dashboard map[string]any, tag string) bool {
	tags, _ := dashboard["tags"].([]any)
	for _, t := range tags {
		if s, ok := t.(string); ok && s == tag {
			return true
		}
	}
	return false
}
