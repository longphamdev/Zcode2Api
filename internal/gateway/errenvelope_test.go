package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// OpenAI clients branch on error.type, and the value differs by status: a
// client-side mistake is invalid_request_error, a provider fault is api_error.
// Sending one string for everything makes that branch unusable.
func TestWriteOpenAIErrorTypeMatchesStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"bad request", 400, "invalid_request_error"},
		{"unprocessable", 422, "invalid_request_error"},
		{"missing auth", 401, "authentication_error"},
		{"forbidden", 403, "permission_error"},
		{"not found", 404, "not_found_error"},
		{"rate limited", 429, "rate_limit_error"},
		{"upstream bad gateway", 502, "api_error"},
		{"unavailable", 503, "api_error"},
		{"overloaded", 529, "api_error"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeOpenAIError(rec, c.status, "boom")

			var out struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
			}
			if out.Error.Type != c.want {
				t.Errorf("status %d: type = %q, want %q", c.status, out.Error.Type, c.want)
			}
			if out.Error.Message != "boom" {
				t.Errorf("message = %q, want %q", out.Error.Message, "boom")
			}
			if rec.Code != c.status {
				t.Errorf("HTTP status = %d, want %d", rec.Code, c.status)
			}
		})
	}
}
