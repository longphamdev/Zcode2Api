package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The five request shapes the ticket defines. `strict` lives at
// response_format.json_schema.strict — not at the top level — and is optional,
// so most clients that omit it must keep working.
func TestClassifyResponseFormat(t *testing.T) {
	cases := []struct {
		name string
		body string
		want responseFormatKind
	}{
		{"absent", `{"model":"m","messages":[]}`, rfNone},
		{"null", `{"model":"m","response_format":null}`, rfNone},
		{"type text", `{"model":"m","response_format":{"type":"text"}}`, rfNone},
		{"json_object", `{"model":"m","response_format":{"type":"json_object"}}`, rfDropped},
		{"json_schema, strict absent",
			`{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"w","schema":{}}}}`, rfDropped},
		{"json_schema, strict false",
			`{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"w","strict":false}}}`, rfDropped},
		{"json_schema, strict null",
			`{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"w","strict":null}}}`, rfDropped},
		{"json_schema, strict true",
			`{"model":"m","response_format":{"type":"json_schema","json_schema":{"name":"w","strict":true}}}`, rfUnsupported},
		{"strict at top level is not the field",
			`{"model":"m","response_format":{"type":"json_schema","strict":true}}`, rfDropped},
		{"unparseable body", `{not json`, rfNone},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyResponseFormat([]byte(c.body)); got != c.want {
				t.Errorf("ClassifyResponseFormat(%s) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// A malformed response_format must not be mistaken for strict: true. Rejecting
// on garbage would break clients over a field we were going to drop anyway.
func TestClassifyResponseFormatMalformedIsNotStrict(t *testing.T) {
	cases := []string{
		`{"model":"m","response_format":"json_object"}`,
		`{"model":"m","response_format":{"type":"json_schema","json_schema":"nope"}}`,
		`{"model":"m","response_format":{"type":123}}`,
		`{"model":"m","response_format":{"type":"json_schema","json_schema":{"strict":"true"}}}`,
	}
	for _, body := range cases {
		if got := ClassifyResponseFormat([]byte(body)); got == rfUnsupported {
			t.Errorf("ClassifyResponseFormat(%s) = rfUnsupported; a malformed shape must not reject", body)
		}
	}
}

// A response_format we cannot fully parse still asked for something, so it must
// warn rather than vanish. Only "text" and a missing field are silent. The one
// exception is a body too broken to parse at all — the conversion rejects that
// on its own, so there is nothing to warn about.
func TestClassifyResponseFormatWarnsForUnparseableShapes(t *testing.T) {
	cases := []string{
		`{"model":"m","response_format":"json_object"}`,
		`{"model":"m","response_format":{"type":"json_schema","json_schema":"nope"}}`,
		`{"model":"m","response_format":{"type":123}}`,
		`{"model":"m","response_format":{"type":"json_schema","json_schema":{"strict":"true"}}}`,
		`{"model":"m","response_format":{"type":"grammar","grammar":{"type":"string"}}}`,
		`{"model":"m","response_format":{}}`,
	}
	for _, body := range cases {
		if got := ClassifyResponseFormat([]byte(body)); got != rfDropped {
			t.Errorf("ClassifyResponseFormat(%s) = %v, want rfDropped (warn, do not stay silent)", body, got)
		}
	}
}

// postChat drives the handler the way a client would.
// The rejection path touches no pool or upstream, so a bare server suffices.
func postChat(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleChatCompletions(rec, req)
	return rec
}

// strict: true must be refused before anything reaches the origin, with an
// OpenAI-shaped error envelope — never plain text, never an Anthropic body.
func TestChatCompletionsRejectsStrictResponseFormat(t *testing.T) {
	body := `{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"w","strict":true,` +
		`"schema":{"type":"object","properties":{"a":{"type":"string"}}}}}}`

	rec := postChat(t, &Server{}, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	var out struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if out.Error == nil {
		t.Fatalf("body has no error envelope: %s", rec.Body.String())
	}
	if out.Error.Type == "" || out.Error.Message == "" {
		t.Errorf("error envelope is incomplete: %+v", *out.Error)
	}
	if !strings.Contains(out.Error.Message, "response_format.json_schema.strict") {
		t.Errorf("message should name the wire field that is unsupported, got %q", out.Error.Message)
	}
}

// The shapes that must NOT be rejected: absent, text, json_object, and
// json_schema without strict. Asserting via the classifier keeps this a unit
// test — the handler path past the classifier needs a live pool, and the point
// here is only that these requests are not refused.
func TestResponseFormatShapesNotRejected(t *testing.T) {
	for _, body := range []string{
		`{"model":"GLM-5.3","messages":[]}`,
		`{"model":"GLM-5.3","messages":[],"response_format":{"type":"text"}}`,
		`{"model":"GLM-5.3","messages":[],"response_format":{"type":"json_object"}}`,
		`{"model":"GLM-5.3","messages":[],"response_format":{"type":"json_schema","json_schema":{"name":"w","schema":{}}}}`,
		`{"model":"GLM-5.3","messages":[],"response_format":{"type":"json_schema","json_schema":{"name":"w","strict":false}}}`,
	} {
		if got := ClassifyResponseFormat([]byte(body)); got == rfUnsupported {
			t.Errorf("shape should not be rejected: %s", body)
		}
	}
}

// The rejection must not fire for an Anthropic-shaped request, which has no
// response_format concept and goes through a different entry point entirely.
func TestClassifyIgnoresAnthropicBody(t *testing.T) {
	body := `{"model":"GLM-5.3","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`
	if got := ClassifyResponseFormat([]byte(body)); got != rfNone {
		t.Errorf("output_config is not response_format; got %v, want rfNone", got)
	}
}
