package symphony

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type HTTPServer struct {
	Orchestrator *Orchestrator
	Logger       *slog.Logger
}

func StartHTTPServer(ctx context.Context, host string, port int, orchestrator *Orchestrator, logger *slog.Logger) (*http.Server, string, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, fmt.Sprint(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	actual := "http://" + ln.Addr().String()
	handler := HTTPServer{Orchestrator: orchestrator, Logger: logger}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			if logger != nil {
				logger.Error("http server failed", "error", err)
			}
		}
	}()
	return server, actual, nil
}

func (s HTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		s.dashboard(w, r)
	case r.URL.Path == "/api/v1/state":
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, s.Orchestrator.Snapshot())
	case r.URL.Path == "/api/v1/refresh":
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w)
			return
		}
		requestedAt := time.Now().UTC()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			_ = s.Orchestrator.Tick(ctx)
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{
			"queued":       true,
			"coalesced":    false,
			"requested_at": requestedAt,
			"operations":   []string{"poll", "reconcile"},
		})
	case strings.HasPrefix(r.URL.Path, "/api/v1/"):
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		identifier, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/v1/"))
		if err != nil || identifier == "" || identifier == "state" || identifier == "refresh" || path.Clean(identifier) != identifier {
			writeAPIError(w, http.StatusNotFound, "issue_not_found", "issue not found")
			return
		}
		detail, ok := s.issueDetail(identifier)
		if !ok {
			writeAPIError(w, http.StatusNotFound, "issue_not_found", "issue not found")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "not found")
	}
}

func (s HTTPServer) dashboard(w http.ResponseWriter, r *http.Request) {
	snapshot := s.Orchestrator.Snapshot()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\"><title>Symphony</title>")
	b.WriteString("<style>body{font-family:system-ui,-apple-system,Segoe UI,sans-serif;margin:24px;color:#1f2937;background:#f8fafc}table{border-collapse:collapse;width:100%;background:white}th,td{padding:8px 10px;border-bottom:1px solid #e5e7eb;text-align:left}code{background:#eef2ff;padding:2px 4px;border-radius:4px}.panel{margin:16px 0}.muted{color:#64748b}</style>")
	b.WriteString("</head><body><h1>Symphony</h1>")
	b.WriteString("<p class=\"muted\">Generated at " + html.EscapeString(snapshot.GeneratedAt.Format(time.RFC3339)) + "</p>")
	b.WriteString(fmt.Sprintf("<p>Running: <strong>%d</strong> &nbsp; Retrying: <strong>%d</strong> &nbsp; Tokens: <strong>%d</strong></p>", snapshot.Counts["running"], snapshot.Counts["retrying"], snapshot.OpenCodeTotals.TotalTokens))
	b.WriteString("<div class=\"panel\"><h2>Running</h2><table><thead><tr><th>Issue</th><th>State</th><th>Session</th><th>Turns</th><th>Last event</th></tr></thead><tbody>")
	for _, row := range snapshot.Running {
		issue := html.EscapeString(row.IssueIdentifier)
		if row.IssueURL != "" {
			issue = "<a href=\"" + html.EscapeString(row.IssueURL) + "\">" + issue + "</a>"
		}
		b.WriteString("<tr><td>" + issue + "</td><td>" + html.EscapeString(row.State) + "</td><td><code>" + html.EscapeString(row.SessionID) + "</code></td><td>" + fmt.Sprint(row.TurnCount) + "</td><td>" + html.EscapeString(row.LastEvent) + "</td></tr>")
	}
	b.WriteString("</tbody></table></div>")
	b.WriteString("<div class=\"panel\"><h2>Retrying</h2><table><thead><tr><th>Issue</th><th>Attempt</th><th>Due</th><th>Error</th></tr></thead><tbody>")
	for _, row := range snapshot.Retrying {
		b.WriteString("<tr><td>" + html.EscapeString(row.Identifier) + "</td><td>" + fmt.Sprint(row.Attempt) + "</td><td>" + html.EscapeString(row.DueAt.Format(time.RFC3339)) + "</td><td>" + html.EscapeString(row.Error) + "</td></tr>")
	}
	b.WriteString("</tbody></table></div></body></html>")
	_, _ = w.Write([]byte(b.String()))
	_ = r
}

func (s HTTPServer) issueDetail(identifier string) (map[string]any, bool) {
	snapshot := s.Orchestrator.Snapshot()
	cfg := s.Orchestrator.config()
	workspacePath := filepath.Join(cfg.Workspace.Root, SanitizeWorkspaceKey(identifier))
	for _, row := range snapshot.Running {
		if row.IssueIdentifier == identifier {
			return map[string]any{
				"issue_identifier": identifier,
				"issue_id":         row.IssueID,
				"status":           "running",
				"workspace":        map[string]any{"path": workspacePath},
				"running":          row,
				"retry":            nil,
				"recent_events":    snapshot.RecentEvents[row.IssueID],
				"last_error":       snapshot.LastErrors[row.IssueID],
				"tracked":          map[string]any{},
			}, true
		}
	}
	for _, row := range snapshot.Retrying {
		if row.Identifier == identifier {
			return map[string]any{
				"issue_identifier": identifier,
				"issue_id":         row.IssueID,
				"status":           "retrying",
				"workspace":        map[string]any{"path": workspacePath},
				"running":          nil,
				"retry":            row,
				"recent_events":    snapshot.RecentEvents[row.IssueID],
				"last_error":       snapshot.LastErrors[row.IssueID],
				"tracked":          map[string]any{},
			}, true
		}
	}
	return nil, false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}
