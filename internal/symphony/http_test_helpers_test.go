package symphony

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func testJSONResponse(t *testing.T, status int, headers map[string]string, value any) *http.Response {
	t.Helper()
	var b strings.Builder
	if err := json.NewEncoder(&b).Encode(value); err != nil {
		t.Fatal(err)
	}
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(b.String())),
	}
}
