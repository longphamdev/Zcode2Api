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

// originStream drives writeOpenAIFromOrigin with a 200 event stream, in either
// client mode.
func originStream(t *testing.T, sse string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	var scn usageScanner
	tee := io.NopCloser(io.TeeReader(strings.NewReader(sse), &scn))
	resp := &http.Response{StatusCode: 200, Body: tee}
	rec := httptest.NewRecorder()
	srv := &Server{}
	srv.writeOpenAIFromOrigin(rec, resp, []byte(sse), tee, "glm-5.3", stream, &scn, false)
	return rec
}

func sseEvent(ev, data string) string { return "event: " + ev + "\n" + "data: " + data + "\n\n" }

var (
	sseStart     = sseEvent("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5}}}`)
	sseText      = sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial internal-host-42"}}`)
	sseStopDelta = sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
	sseStop      = sseEvent("message_stop", `{"type":"message_stop"}`)
	sseError     = sseEvent("error", `{"type":"error","error":{"type":"overloaded_error","message":"origin is overloaded"}}`)
)

// An Origin error before any chunk has reached the client is an ordinary 502:
// the response is not committed until the first chunk is written, and SDKs
// branch on the status before they parse a single SSE line.
func TestStreamErrorBeforeFirstChunkIsBadGateway(t *testing.T) {
	rec := originStream(t, streamWithErrorEvent(t), true)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not a JSON envelope: %v (%s)", err, rec.Body.String())
	}
	if out.Error.Message != "origin is overloaded" {
		t.Errorf("message = %q, want the Origin's own", out.Error.Message)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("a failed stream must not end in [DONE]")
	}
}

// Once a chunk is out the status is spent; the error travels as one SSE error
// frame, and the stream must not end in [DONE].
func TestStreamErrorAfterFirstChunkIsErrorFrame(t *testing.T) {
	rec := originStream(t, sseStart+sseText+sseError, true)

	frames, done := streamFrames(t, rec.Body.String())
	if done {
		t.Error("a failed stream must not end in [DONE]")
	}
	var errFrames int
	for _, fr := range frames {
		if e, ok := fr["error"].(map[string]any); ok {
			errFrames++
			if e["message"] != "origin is overloaded" {
				t.Errorf("error.message = %v, want the Origin's own", e["message"])
			}
		}
	}
	if errFrames != 1 {
		t.Errorf("got %d error frames, want exactly 1 (body=%s)", errFrames, rec.Body.String())
	}
}

// A stream that stops before a finish trigger is not a reply. It used to get
// [DONE] regardless, so a connection cut mid-answer looked like a short answer.
func TestStreamEndingWithoutFinishIsNotClean(t *testing.T) {
	t.Run("nothing written yet: 502", func(t *testing.T) {
		rec := originStream(t, sseStart, true)
		if rec.Code != http.StatusBadGateway {
			t.Errorf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "[DONE]") {
			t.Error("must not end in [DONE]")
		}
	})
	t.Run("content written: error frame, no [DONE]", func(t *testing.T) {
		rec := originStream(t, sseStart+sseText, true)
		frames, done := streamFrames(t, rec.Body.String())
		if done {
			t.Error("must not end in [DONE]")
		}
		if len(frames) == 0 {
			t.Fatal("no frames at all")
		}
		if _, ok := frames[len(frames)-1]["error"]; !ok {
			t.Errorf("last frame is not an error frame: %v", frames[len(frames)-1])
		}
	})
}

// Either finish trigger alone is enough: the Origin's message_delta carries the
// stop_reason, and message_stop follows, but a stream with only one of them is
// still a finished message.
func TestStreamEitherFinishTriggerIsClean(t *testing.T) {
	for name, tail := range map[string]string{"stop_reason only": sseStopDelta, "message_stop only": sseStop, "both": sseStopDelta + sseStop} {
		t.Run(name, func(t *testing.T) {
			rec := originStream(t, sseStart+sseText+tail, true)
			frames, done := streamFrames(t, rec.Body.String())
			if !done {
				t.Errorf("a finished stream must end in [DONE] (body=%s)", rec.Body.String())
			}
			for _, fr := range frames {
				if _, ok := fr["error"]; ok {
					t.Errorf("unexpected error frame: %v", fr)
				}
			}
		})
	}
}

// The client asked for a non-stream answer and the Origin streamed. Collecting
// an error event, or a stream that stopped early, into an empty completion is
// the same lie as [DONE] on the streaming path, under a different shape.
func TestCollectedStreamFailureIsBadGateway(t *testing.T) {
	cases := []struct {
		name    string
		sse     string
		wantMsg string
	}{
		{"error event", streamWithErrorEvent(t), "origin is overloaded"},
		{"error after content", sseStart + sseText + sseError, "origin is overloaded"},
		{"ended without finish", sseStart + sseText, "origin returned a malformed response"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := originStream(t, c.sse, false)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
			}
			var out struct {
				Error   *struct{ Message string } `json:"error"`
				Choices []any                     `json:"choices"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
			}
			if out.Error == nil || out.Error.Message != c.wantMsg {
				t.Errorf("error = %+v, want message %q", out.Error, c.wantMsg)
			}
			if out.Choices != nil {
				t.Error("a failed collection must not carry choices")
			}
			if strings.Contains(rec.Body.String(), "internal-host-42") {
				t.Error("partial content leaked into the error response")
			}
		})
	}

	t.Run("finished stream still renders", func(t *testing.T) {
		rec := originStream(t, sseStart+sseText+sseStopDelta+sseStop, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"chat.completion"`) {
			t.Errorf("not a completion: %s", rec.Body.String())
		}
	})
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
		// Valid JSON that is not a message document. These decode without error
		// into an empty document and used to become an empty completion.
		{"json null", `null`},
		{"empty object", `{}`},
		{"empty array", `[]`},
		{"unrelated object", `{"ok":true,"host":"internal-host-42"}`},
		{"error document under 200", `{"type":"error","error":{"type":"overloaded_error","message":"internal-host-42"}}`},
		{"content is not an array", `{"type":"message","content":"internal-host-42","stop_reason":"end_turn"}`},
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

// The floor for "is a message document" is a content array. Fields the Origin
// may omit or null — type, stop_reason — must not be required, or a real reply
// is answered 502.
func TestMessageDocumentWithoutOptionalFieldsStillRenders(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"stop_reason null, type present", `{"type":"message","stop_reason":null,"content":[{"type":"text","text":"hi"}]}`},
		{"no type, no stop_reason", `{"content":[{"type":"text","text":"hi"}]}`},
		{"empty content array", `{"type":"message","content":[],"stop_reason":"end_turn"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := origin4xxReply(t, 200, c.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
			}
			if out["object"] != "chat.completion" {
				t.Errorf("object = %v, want chat.completion", out["object"])
			}
		})
	}
}

// The request log must record what the client got, which the Origin's status no
// longer tells: a 2xx the gateway could not render went out as 502, and a stream
// that failed after its first chunk kept its 200.
func TestOriginRenderingReportsWhatTheClientGot(t *testing.T) {
	run := func(body string, stream bool) (int, bool) {
		var scn usageScanner
		tee := io.NopCloser(io.TeeReader(strings.NewReader(body), &scn))
		resp := &http.Response{StatusCode: 200, Body: tee}
		srv := &Server{}
		return srv.writeOpenAIFromOrigin(httptest.NewRecorder(), resp, []byte(body), tee, "glm-5.3", stream, &scn, false)
	}
	cases := []struct {
		name       string
		body       string
		stream     bool
		wantStatus int
		wantOK     bool
	}{
		{"json reply", `{"type":"message","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}]}`, false, 200, true},
		{"json garbage under 200", `{}`, false, 502, false},
		{"stream finished", sseStart + sseText + sseStop, true, 200, true},
		{"stream failed before first chunk", sseStart + sseError, true, 502, false},
		{"stream failed after first chunk", sseStart + sseText + sseError, true, 200, false},
		{"collected stream failed", sseStart + sseText, false, 502, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, ok := run(c.body, c.stream)
			if status != c.wantStatus || ok != c.wantOK {
				t.Errorf("reported (%d, %v), want (%d, %v)", status, ok, c.wantStatus, c.wantOK)
			}
		})
	}
}
