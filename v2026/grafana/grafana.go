package grafana

// default grafana dashboards for the urnetwork services,
// loaded into the grafana service with `bringyourctl grafana load-defaults`.
// the dashboards reference the provisioned datasource uids warp-loki and
// warp-mimir (see warp/grafana in the warp repo), and have stable uids so
// that loading is an idempotent upsert into the urnetwork folder

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

//go:embed dashboards/*.json
var dashboardsFs embed.FS

const FolderUid = "urnetwork"
const FolderTitle = "urnetwork"

// LoadDefaults upserts the default dashboards into the grafana
// at grafanaUrl, in the urnetwork folder.
// returns the loaded dashboard titles
func LoadDefaults(ctx context.Context, grafanaUrl string, username string, password string) ([]string, error) {
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
		return nil, err
	}
	if statusCode == 404 {
		statusCode, responseBody, err := do("POST", "/api/folders", map[string]any{
			"uid":   FolderUid,
			"title": FolderTitle,
		})
		if err != nil {
			return nil, err
		}
		if 400 <= statusCode {
			return nil, errors.New(fmt.Sprintf("Could not create folder (%d): %s", statusCode, string(responseBody)))
		}
	} else if 400 <= statusCode {
		return nil, errors.New(fmt.Sprintf("Could not read folder (%d).", statusCode))
	}

	entries, err := dashboardsFs.ReadDir("dashboards")
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	titles := []string{}
	for _, name := range names {
		dashboardJson, err := dashboardsFs.ReadFile(fmt.Sprintf("dashboards/%s", name))
		if err != nil {
			return nil, err
		}
		dashboard := map[string]any{}
		if err := json.Unmarshal(dashboardJson, &dashboard); err != nil {
			return nil, errors.New(fmt.Sprintf("Could not parse %s (%s)", name, err))
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
			return nil, err
		}
		if 400 <= statusCode {
			return nil, errors.New(fmt.Sprintf("Could not load %s (%d): %s", name, statusCode, string(responseBody)))
		}

		title, _ := dashboard["title"].(string)
		titles = append(titles, title)
	}
	return titles, nil
}
