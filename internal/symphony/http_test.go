package symphony

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPStateAndRefreshEndpoints(t *testing.T) {
	cfg := testRuntimeConfig(t.TempDir())
	tracker := newFakeTracker(nil)
	orch := NewOrchestrator(cfg, WorkflowDefinition{PromptTemplate: "work"}, tracker, &fakeRunner{}, nil)
	handler := HTTPServer{Orchestrator: orch}

	stateReq := httptest.NewRequest(http.MethodGet, "/api/v1/state", nil)
	stateResp := httptest.NewRecorder()
	handler.ServeHTTP(stateResp, stateReq)
	if stateResp.Code != http.StatusOK {
		t.Fatalf("state status = %d body=%s", stateResp.Code, stateResp.Body.String())
	}
	var state RuntimeSnapshot
	if err := json.Unmarshal(stateResp.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Counts["running"] != 0 || state.Counts["retrying"] != 0 {
		t.Fatalf("unexpected state counts: %#v", state.Counts)
	}

	refreshReq := httptest.NewRequest(http.MethodPost, "/api/v1/refresh", nil)
	refreshResp := httptest.NewRecorder()
	handler.ServeHTTP(refreshResp, refreshReq)
	if refreshResp.Code != http.StatusAccepted {
		t.Fatalf("refresh status = %d body=%s", refreshResp.Code, refreshResp.Body.String())
	}

	badReq := httptest.NewRequest(http.MethodPut, "/api/v1/state", nil)
	badResp := httptest.NewRecorder()
	handler.ServeHTTP(badResp, badReq)
	if badResp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("bad method status = %d", badResp.Code)
	}

	time.Sleep(10 * time.Millisecond)
}
