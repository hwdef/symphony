package symphony

import (
	"net/http"
	"strings"
	"testing"
)

func TestGitLabCandidateFetchPaginationAndNormalization(t *testing.T) {
	var sawPage2 bool
	var sawRefreshIIDs bool
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/api/v4/projects/123/issues" && r.URL.Query().Get("page") == "1":
			if r.Header.Get("PRIVATE-TOKEN") != "token" {
				t.Errorf("missing PRIVATE-TOKEN header")
			}
			if r.URL.Query().Get("state") != "opened" {
				t.Errorf("candidate state query = %q", r.URL.Query().Get("state"))
			}
			return testJSONResponse(t, http.StatusOK, map[string]string{"X-Next-Page": "2"}, []map[string]any{
				{
					"id": 101, "iid": 1, "title": "First", "description": "Body", "state": "opened",
					"labels":  []string{"Status::Todo", "Ready"},
					"web_url": "https://gitlab.example.com/p/-/issues/1",
				},
				{
					"id": 102, "iid": 2, "title": "Done", "state": "opened",
					"labels": []string{"Status::Done", "Ready"},
				},
			}), nil
		case r.URL.Path == "/api/v4/projects/123/issues" && r.URL.Query().Get("page") == "2":
			sawPage2 = true
			return testJSONResponse(t, http.StatusOK, nil, []map[string]any{
				{
					"id": 103, "iid": 3, "title": "No label", "state": "opened",
					"labels": []string{"Status::In Progress"},
				},
			}), nil
		case r.URL.Path == "/api/v4/projects/123/issues/1/links":
			return testJSONResponse(t, http.StatusOK, nil, []map[string]any{
				{
					"link_type": "is_blocked_by",
					"issue": map[string]any{
						"id": 99, "iid": 9, "title": "Blocker", "state": "closed",
						"labels": []string{"Status::Done"},
					},
				},
			}), nil
		case r.URL.Path == "/api/v4/projects/123/issues" && len(r.URL.Query()["iids[]"]) > 0:
			if strings.Join(r.URL.Query()["iids[]"], ",") == "1" {
				sawRefreshIIDs = true
			}
			return testJSONResponse(t, http.StatusOK, nil, []map[string]any{
				{"id": 101, "iid": 1, "title": "First", "state": "opened", "labels": []string{"Status::In Progress", "Ready"}},
			}), nil
		default:
			t.Errorf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
			return testJSONResponse(t, http.StatusNotFound, nil, map[string]any{"error": "not found"}), nil
		}
	})

	client := NewGitLabClient(TrackerConfig{
		Endpoint:         "https://gitlab.test/api/v4",
		APIKey:           "token",
		ProjectID:        "123",
		StateLabelPrefix: "Status::",
		RequiredLabels:   []string{"ready"},
		ActiveStates:     []string{"Todo", "In Progress"},
		TerminalStates:   []string{"Done", "Closed"},
	})
	client.HTTP = &http.Client{Transport: transport}
	issues, err := client.FetchCandidateIssues(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !sawPage2 {
		t.Fatalf("pagination did not request page 2")
	}
	if len(issues) != 1 {
		t.Fatalf("candidate issues len = %d: %#v", len(issues), issues)
	}
	issue := issues[0]
	if issue.ID != "101" || issue.Identifier != "#1" || issue.State != "Todo" {
		t.Fatalf("unexpected issue normalization: %#v", issue)
	}
	if len(issue.Labels) != 2 || issue.Labels[0] != "status::todo" || issue.Labels[1] != "ready" {
		t.Fatalf("labels not normalized: %#v", issue.Labels)
	}
	if len(issue.BlockedBy) != 1 || issue.BlockedBy[0].State == nil || *issue.BlockedBy[0].State != "closed" {
		t.Fatalf("blockers not normalized: %#v", issue.BlockedBy)
	}

	refreshed, err := client.FetchIssueStatesByIDs(t.Context(), []string{"101"})
	if err != nil {
		t.Fatal(err)
	}
	if !sawRefreshIIDs {
		t.Fatalf("refresh did not use cached iids[]")
	}
	if len(refreshed) != 1 || refreshed[0].State != "In Progress" {
		t.Fatalf("unexpected refresh: %#v", refreshed)
	}
}

func TestGitLabEmptyTerminalFetchDoesNotCallAPI(t *testing.T) {
	called := false
	client := NewGitLabClient(TrackerConfig{Endpoint: "https://gitlab.test/api/v4", ProjectID: "123", APIKey: "token"})
	client.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return testJSONResponse(t, http.StatusNotFound, nil, nil), nil
	})}
	issues, err := client.FetchIssuesByStates(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatalf("empty terminal fetch called API")
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
}
