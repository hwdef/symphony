package symphony

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type GitLabClient struct {
	Config TrackerConfig
	HTTP   *http.Client

	mu      sync.Mutex
	idToIID map[string]int
}

func NewGitLabClient(cfg TrackerConfig) *GitLabClient {
	return &GitLabClient{
		Config:  cfg,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		idToIID: map[string]int{},
	}
}

func (c *GitLabClient) FetchCandidateIssues(ctx context.Context) ([]Issue, error) {
	if c.Config.ProjectID == "" {
		return nil, ErrMissingTrackerProjectID
	}
	rawIssues, err := c.fetchIssuePages(ctx, func(values url.Values) {
		values.Set("state", "opened")
		values.Set("scope", "all")
		applyAssignee(values, c.Config.Assignee)
	})
	if err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(rawIssues))
	for _, raw := range rawIssues {
		issue := c.normalizeIssue(raw)
		if !containsState(c.Config.ActiveStates, issue.State) {
			continue
		}
		if containsState(c.Config.TerminalStates, issue.State) {
			continue
		}
		if !issueHasRequiredLabels(issue.Labels, c.Config.RequiredLabels) {
			continue
		}
		blockers, err := c.fetchBlockers(ctx, raw.IID)
		if err != nil {
			return nil, err
		}
		issue.BlockedBy = blockers
		issues = append(issues, issue)
		c.rememberIID(issue.ID, raw.IID)
	}
	return issues, nil
}

func (c *GitLabClient) FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	if len(states) == 0 {
		return []Issue{}, nil
	}
	seen := map[string]Issue{}
	for _, state := range states {
		state = strings.TrimSpace(state)
		if state == "" {
			continue
		}
		if normalizeState(state) == "closed" {
			rawIssues, err := c.fetchIssuePages(ctx, func(values url.Values) {
				values.Set("state", "closed")
				values.Set("scope", "all")
			})
			if err != nil {
				return nil, err
			}
			for _, raw := range rawIssues {
				issue := c.normalizeIssue(raw)
				seen[issue.ID] = issue
				c.rememberIID(issue.ID, raw.IID)
			}
			continue
		}
		rawIssues, err := c.fetchIssuePages(ctx, func(values url.Values) {
			values.Set("scope", "all")
			values.Set("labels", c.Config.StateLabelPrefix+state)
		})
		if err != nil {
			return nil, err
		}
		for _, raw := range rawIssues {
			issue := c.normalizeIssue(raw)
			seen[issue.ID] = issue
			c.rememberIID(issue.ID, raw.IID)
		}
	}
	out := make([]Issue, 0, len(seen))
	for _, issue := range seen {
		out = append(out, issue)
	}
	return out, nil
}

func (c *GitLabClient) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	if len(ids) == 0 {
		return []Issue{}, nil
	}
	values := url.Values{}
	values.Set("scope", "all")
	for _, id := range ids {
		iid, ok := c.lookupIID(id)
		if !ok {
			continue
		}
		values.Add("iids[]", strconv.Itoa(iid))
	}
	if len(values["iids[]"]) == 0 {
		return []Issue{}, nil
	}
	rawIssues, err := c.getIssues(ctx, values)
	if err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(rawIssues))
	for _, raw := range rawIssues {
		issue := c.normalizeIssue(raw)
		out = append(out, issue)
		c.rememberIID(issue.ID, raw.IID)
	}
	return out, nil
}

func (c *GitLabClient) fetchIssuePages(ctx context.Context, decorate func(url.Values)) ([]gitlabIssue, error) {
	var all []gitlabIssue
	page := 1
	for {
		values := url.Values{}
		values.Set("per_page", "100")
		values.Set("page", strconv.Itoa(page))
		if decorate != nil {
			decorate(values)
		}
		req, err := c.newRequest(ctx, http.MethodGet, "/projects/"+gitlabProjectPathSegment(c.Config.ProjectID)+"/issues", values, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return nil, fmt.Errorf("gitlab_api_request: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("gitlab_api_request: %w", readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("gitlab_api_status: status=%d body=%s", resp.StatusCode, truncateString(string(body), 2048))
		}
		var pageIssues []gitlabIssue
		if err := json.Unmarshal(body, &pageIssues); err != nil {
			return nil, fmt.Errorf("gitlab_unknown_payload: %w", err)
		}
		all = append(all, pageIssues...)
		next := strings.TrimSpace(resp.Header.Get("X-Next-Page"))
		if next == "" {
			break
		}
		n, err := strconv.Atoi(next)
		if err != nil || n <= page {
			return nil, fmt.Errorf("gitlab_pagination_error: invalid X-Next-Page %q", next)
		}
		page = n
	}
	return all, nil
}

func (c *GitLabClient) getIssues(ctx context.Context, values url.Values) ([]gitlabIssue, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/projects/"+gitlabProjectPathSegment(c.Config.ProjectID)+"/issues", values, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab_api_request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("gitlab_api_request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitlab_api_status: status=%d body=%s", resp.StatusCode, truncateString(string(body), 2048))
	}
	var issues []gitlabIssue
	if err := json.Unmarshal(body, &issues); err != nil {
		return nil, fmt.Errorf("gitlab_unknown_payload: %w", err)
	}
	return issues, nil
}

func (c *GitLabClient) fetchBlockers(ctx context.Context, iid int) ([]BlockerRef, error) {
	if iid == 0 {
		return nil, nil
	}
	path := fmt.Sprintf("/projects/%s/issues/%d/links", gitlabProjectPathSegment(c.Config.ProjectID), iid)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("gitlab_api_request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("gitlab_api_request: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitlab_api_status: status=%d body=%s", resp.StatusCode, truncateString(string(body), 2048))
	}
	var links []gitlabIssueLink
	if err := json.Unmarshal(body, &links); err != nil {
		return nil, fmt.Errorf("gitlab_unknown_payload: %w", err)
	}
	blockers := make([]BlockerRef, 0, len(links))
	for _, link := range links {
		if link.LinkType != "is_blocked_by" {
			continue
		}
		linked := link.Issue
		if linked.ID == 0 && link.ID != 0 {
			linked = gitlabIssue{
				ID:        link.ID,
				IID:       link.IID,
				State:     link.State,
				Labels:    link.Labels,
				WebURL:    link.WebURL,
				Title:     link.Title,
				CreatedAt: link.CreatedAt,
				UpdatedAt: link.UpdatedAt,
			}
		}
		issue := c.normalizeIssue(linked)
		id := issue.ID
		identifier := issue.Identifier
		state := issue.State
		blockers = append(blockers, BlockerRef{ID: &id, Identifier: &identifier, State: &state})
		c.rememberIID(id, linked.IID)
	}
	return blockers, nil
}

func (c *GitLabClient) newRequest(ctx context.Context, method, path string, values url.Values, body io.Reader) (*http.Request, error) {
	if c.Config.Endpoint == "" {
		c.Config.Endpoint = "https://gitlab.com/api/v4"
	}
	u, err := url.Parse(strings.TrimRight(c.Config.Endpoint, "/") + path)
	if err != nil {
		return nil, err
	}
	if values != nil {
		u.RawQuery = values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if c.Config.APIKey != "" {
		req.Header.Set("PRIVATE-TOKEN", c.Config.APIKey)
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *GitLabClient) normalizeIssue(raw gitlabIssue) Issue {
	id := strconv.FormatInt(raw.ID, 10)
	identifier := "#" + strconv.Itoa(raw.IID)
	state := c.deriveState(raw.State, raw.Labels)
	labels := normalizeLabels(raw.Labels)
	issue := Issue{
		ID:          id,
		Identifier:  identifier,
		Title:       raw.Title,
		Description: stringPtrOrNil(raw.Description),
		State:       state,
		URL:         stringPtrOrNil(raw.WebURL),
		Labels:      labels,
		BlockedBy:   nil,
		CreatedAt:   parseTimePtr(raw.CreatedAt),
		UpdatedAt:   parseTimePtr(raw.UpdatedAt),
	}
	return issue
}

func (c *GitLabClient) deriveState(native string, labels []string) string {
	if normalizeState(native) == "closed" {
		return "closed"
	}
	prefix := c.Config.StateLabelPrefix
	if prefix == "" {
		prefix = "Status::"
	}
	for _, label := range labels {
		if strings.HasPrefix(label, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(label, prefix))
		}
	}
	if native == "" {
		return "opened"
	}
	return strings.ToLower(native)
}

func (c *GitLabClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *GitLabClient) rememberIID(id string, iid int) {
	if id == "" || iid == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idToIID == nil {
		c.idToIID = map[string]int{}
	}
	c.idToIID[id] = iid
}

func (c *GitLabClient) lookupIID(id string) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	iid, ok := c.idToIID[id]
	return iid, ok
}

func applyAssignee(values url.Values, assignee string) {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return
	}
	if _, err := strconv.Atoi(assignee); err == nil {
		values.Set("assignee_id", assignee)
	} else {
		values.Set("assignee_username", assignee)
	}
}

func gitlabProjectPathSegment(projectID string) string {
	if strings.Contains(strings.ToLower(projectID), "%2f") {
		return projectID
	}
	return url.PathEscape(projectID)
}

func normalizeLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, strings.ToLower(strings.TrimSpace(label)))
	}
	return out
}

func issueHasRequiredLabels(labels, required []string) bool {
	if len(required) == 0 {
		return true
	}
	present := map[string]bool{}
	for _, label := range labels {
		present[strings.ToLower(strings.TrimSpace(label))] = true
	}
	for _, label := range required {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" || !present[label] {
			return false
		}
	}
	return true
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func parseTimePtr(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}

type gitlabIssue struct {
	ID          int64    `json:"id"`
	IID         int      `json:"iid"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	State       string   `json:"state"`
	Labels      []string `json:"labels"`
	WebURL      string   `json:"web_url"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

type gitlabIssueLink struct {
	LinkType    string      `json:"link_type"`
	Issue       gitlabIssue `json:"issue"`
	ID          int64       `json:"id"`
	IID         int         `json:"iid"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	State       string      `json:"state"`
	Labels      []string    `json:"labels"`
	WebURL      string      `json:"web_url"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at"`
}
