package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zcode2api/internal/config"
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

// The gateway's own 401 came from a hand-written JSON literal in the auth
// middleware, so it carried only two of the four members the spec requires —
// inconsistent with every envelope produced through writeOpenAIErrorDetail.
func TestGatewayAuthEnvelopeCarriesAllRequiredMembers(t *testing.T) {
	srv := &Server{cfg: config.Config{GatewayKey: "right"}}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()

	srv.withGatewayAuth(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the handler must not run for a rejected key")
	})(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	var out struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	for _, k := range []string{"type", "message", "param", "code"} {
		if _, present := out.Error[k]; !present {
			t.Errorf("error.%s is absent; got %s", k, rec.Body.String())
		}
	}
	var typ string
	_ = json.Unmarshal(out.Error["type"], &typ)
	if typ != "authentication_error" {
		t.Errorf("type = %q, want authentication_error", typ)
	}
	var msg string
	_ = json.Unmarshal(out.Error["message"], &msg)
	if msg != "invalid gateway key" {
		t.Errorf("message = %q, want the existing wording unchanged", msg)
	}
}

// A non-2xx reply that is not in the Origin's error shape must not have its body
// echoed to the client. Up to 200 bytes of whatever the Origin said — an HTML
// error page, an internal trace — used to be pasted into error.message.
func TestUpstreamErrorDoesNotEchoRawBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"html error page", `<html><body>502 Bad Gateway<br>nginx/1.24.0</body></html>`},
		{"json that is not the error shape", `{"unexpected":"shape","secret":"internal-host-42"}`},
		{"empty body", ``},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := origin4xxReply(t, 502, c.body)

			var out struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
			}
			for _, leak := range []string{"nginx", "internal-host-42", "<html", "unexpected"} {
				if strings.Contains(out.Error.Message, leak) {
					t.Errorf("message echoes origin body (%q found): %q", leak, out.Error.Message)
				}
			}
			if out.Error.Message == "" {
				t.Error("message is empty; a client needs something readable")
			}
			if out.Error.Type == "" {
				t.Error("type is empty")
			}
			var full map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if _, present := full["choices"]; present {
				t.Error("a non-2xx reply must not carry choices")
			}
		})
	}
}

// A 2xx whose body does not parse as a message document is not a success, and
// its body is not ours to show. It used to be echoed into error.message under
// the Origin's own 200 — an error envelope labelled as success, carrying
// whatever a proxy or an error page had put there.
func TestMalformedSuccessBodyIsBadGatewayWithoutEcho(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"html page", `<html><body>Service temporarily unavailable<br>nginx/1.24.0</body></html>`},
		{"truncated json", `{"id":"msg_1","content":[{"type":"text","text":"internal-host-42`},
		{"plain text", `upstream prematurely closed connection`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := origin4xxReply(t, 200, c.body)

			if rec.Code != http.StatusBadGateway {
				t.Errorf("HTTP status = %d, want 502; a body that is not a reply is not a success", rec.Code)
			}
			var out struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
				Choices []any `json:"choices"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
			}
			for _, leak := range []string{"nginx", "internal-host-42", "<html", "prematurely"} {
				if strings.Contains(out.Error.Message, leak) {
					t.Errorf("message echoes origin body (%q found): %q", leak, out.Error.Message)
				}
			}
			if out.Error.Message == "" {
				t.Error("message is empty; a client needs something readable")
			}
			if out.Error.Type != "api_error" {
				t.Errorf("type = %q, want api_error", out.Error.Type)
			}
			if out.Choices != nil {
				t.Error("must not carry choices")
			}
		})
	}
}
