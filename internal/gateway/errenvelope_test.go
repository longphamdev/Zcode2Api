package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The bundled spec's Error schema marks type, message, param and code all
// required, and chat-error-handling validates the body against ErrorResponse.
// param and code must be present-and-null rather than omitted.
func TestWriteOpenAIErrorCarriesAllRequiredMembers(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIError(rec, 400, "boom")

	var out struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	for _, k := range []string{"type", "message", "param", "code"} {
		raw, present := out.Error[k]
		if !present {
			t.Errorf("error.%s is absent; the spec marks it required", k)
			continue
		}
		if k == "param" || k == "code" {
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Errorf("error.%s is not valid JSON: %v", k, err)
			} else if v != nil {
				t.Errorf("error.%s = %#v, want null when unknown", k, v)
			}
		}
	}
}

// origin4xxReply drives writeOpenAIFromOrigin with an upstream reply that has
// a non-2xx status but a body which parses as valid JSON — the shape that is
// mis-rendered as a completion today.
func origin4xxReply(t *testing.T, status int, body string) *httptest.ResponseRecorder {
	t.Helper()
	var scn usageScanner
	tee := io.NopCloser(io.TeeReader(strings.NewReader(body), &scn))
	resp := &http.Response{StatusCode: status, Body: tee}
	rec := httptest.NewRecorder()
	srv := &Server{}
	srv.writeOpenAIFromOrigin(rec, resp, []byte(body), tee, "glm-5.3", false, &scn, false)
	return rec
}

// An upstream error whose body happens to be valid JSON must not be rendered as
// a completion. This is the content:"" reproduction: origin 400, code 1213,
// body parses, and today the client gets {"choices":[...],"object":"chat.completion"}.
func TestUpstreamErrorIsNotRenderedAsCompletion(t *testing.T) {
	// The origin's error document: valid JSON, and it even carries the fields
	// writeOpenAIFromJSON looks for, which is why it slipped through.
	body := `{"id":"msg_err","type":"error","role":"assistant","stop_reason":null,` +
		`"content":[{"type":"text","text":""}],"error":{"code":1213,"message":"content is required"}}`

	rec := origin4xxReply(t, 400, body)

	if rec.Code != 400 {
		t.Errorf("HTTP status = %d, want 400 (status must be preserved)", rec.Code)
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if _, present := out["choices"]; present {
		t.Errorf("error response must not carry choices; got %s", rec.Body.String())
	}
	if _, present := out["object"]; present {
		if obj, _ := out["object"].(string); obj == "chat.completion" {
			t.Errorf("error response must not be shaped like a completion; got %s", rec.Body.String())
		}
	}
	env, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("body has no error envelope: %s", rec.Body.String())
	}
	if _, ok := env["message"].(string); !ok {
		t.Errorf("error.message missing or not a string: %v", env["message"])
	}
	if _, ok := env["type"].(string); !ok {
		t.Errorf("error.type missing or not a string: %v", env["type"])
	}
	// The origin's code must survive somewhere readable.
	all, _ := json.Marshal(out)
	if !strings.Contains(string(all), "1213") {
		t.Errorf("origin error code 1213 did not survive into the response: %s", all)
	}
}

// A malformed error body must still produce an envelope.
func TestUpstreamErrorWithMalformedBodyIsEnvelope(t *testing.T) {
	rec := origin4xxReply(t, 502, `<html>bad gateway</html>`)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if _, present := out["error"]; !present {
		t.Errorf("body has no error envelope: %s", rec.Body.String())
	}
	if _, present := out["choices"]; present {
		t.Errorf("must not carry choices: %s", rec.Body.String())
	}
}

// A 200 with a valid completion must be untouched by this change.
func TestUpstreamSuccessStillRendersCompletion(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","stop_reason":"end_turn",` +
		`"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":2}}`

	rec := origin4xxReply(t, 200, body)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if out["object"] != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", out["object"])
	}
	if _, present := out["usage"]; !present {
		t.Error("usage is missing from a successful response")
	}
}

// An Origin error can also arrive as a 200-status stream carrying an `error`
// event. Emitting nothing and then [DONE] would look like a successful empty
// answer, which is the same lie the status fix removed for non-stream replies.
func streamWithErrorEvent(t *testing.T) string {
	return strings.Join([]string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}` + "\n\n",
		`event: error` + "\n" + `data: {"type":"error","error":{"type":"overloaded_error","message":"origin is overloaded"}}` + "\n\n",
	}, "")
}

func TestStreamErrorEventReachesClientAsError(t *testing.T) {
	sse := streamWithErrorEvent(t)
	var scn usageScanner
	tee := io.NopCloser(io.TeeReader(strings.NewReader(sse), &scn))
	resp := &http.Response{StatusCode: 200, Body: tee}

	rec := httptest.NewRecorder()
	srv := &Server{}
	srv.writeOpenAIFromOrigin(rec, resp, []byte(sse), tee, "glm-5.3", true, &scn, false)

	body := rec.Body.String()
	if !strings.Contains(body, "origin is overloaded") {
		t.Errorf("the origin's error message did not reach the client; body=%q", body)
	}
	// A bare [DONE] with no error frame is precisely the silent-empty-answer
	// failure this guards.
	if !strings.Contains(body, `"error"`) {
		t.Errorf("stream carried no error frame; body=%q", body)
	}
}
