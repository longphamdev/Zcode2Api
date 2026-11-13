package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The client opts in with stream_options.include_usage. Absent, false, or a
// missing stream_options block all mean "do not send a usage chunk".
func TestParseIncludeUsage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"opted in", `{"stream":true,"stream_options":{"include_usage":true}}`, true},
		{"opted out", `{"stream":true,"stream_options":{"include_usage":false}}`, false},
		{"no stream_options", `{"stream":true}`, false},
		{"empty object", `{}`, false},
		{"null stream_options", `{"stream":true,"stream_options":null}`, false},
		{"malformed body", `{not json`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ParseIncludeUsage([]byte(c.body)); got != c.want {
				t.Errorf("ParseIncludeUsage(%s) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

// usageStreamSSE is a minimal origin stream carrying the token triple on
// message_delta, which is where the origin reports it.
func usageStreamSSE() string {
	return strings.Join([]string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":15,"output_tokens":100,"cache_read_input_tokens":1344}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}, "")
}

// streamFrames splits an SSE response body into the JSON payloads it wrote,
// stopping at [DONE], plus a flag for whether [DONE] was present.
func streamFrames(t *testing.T, body string) (frames []map[string]any, done bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			return frames, true
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("chunk is not JSON: %v (%s)", err, payload)
		}
		frames = append(frames, m)
	}
	return frames, false
}

func runStream(t *testing.T, includeUsage bool) *httptest.ResponseRecorder {
	t.Helper()
	sse := usageStreamSSE()
	head := []byte("event: message_start\n")
	var scn usageScanner
	tee := io.NopCloser(io.TeeReader(strings.NewReader(sse), &scn))
	resp := &http.Response{StatusCode: 200, Body: tee}

	rec := httptest.NewRecorder()
	srv := &Server{}
	srv.writeOpenAIFromOrigin(rec, resp, head, tee, "glm-5.3", true, &scn, includeUsage)
	return rec
}

// When the client asks, the stream ends with a usage chunk: empty choices,
// usage object, immediately before [DONE].
func TestStreamEmitsUsageChunkWhenRequested(t *testing.T) {
	rec := runStream(t, true)
	frames, done := streamFrames(t, rec.Body.String())

	if !done {
		t.Fatal("stream did not end with [DONE]")
	}
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want at least 2", len(frames))
	}

	last := frames[len(frames)-1]
	choices, ok := last["choices"].([]any)
	if !ok {
		t.Fatalf("last chunk choices is not an array: %T", last["choices"])
	}
	if len(choices) != 0 {
		t.Errorf("usage chunk choices = %v, want an empty array", choices)
	}

	u, ok := last["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage chunk has no usage object: %v", last)
	}
	if got := u["prompt_tokens"]; got != float64(1359) {
		t.Errorf("prompt_tokens = %v, want 1359", got)
	}
	if got := u["completion_tokens"]; got != float64(100) {
		t.Errorf("completion_tokens = %v, want 100", got)
	}
	if got := u["total_tokens"]; got != float64(1459) {
		t.Errorf("total_tokens = %v, want 1459", got)
	}

	// Position: nothing with content may follow the usage chunk.
	if obj, _ := last["object"].(string); obj != "chat.completion.chunk" {
		t.Errorf("usage chunk object = %q, want chat.completion.chunk", obj)
	}

	// And the content chunk before it must still be there.
	prev := frames[len(frames)-2]
	prevChoices, _ := prev["choices"].([]any)
	if len(prevChoices) != 1 {
		t.Fatalf("frame before the usage chunk should carry the content delta, got %v", prev)
	}
}

// When the client does not ask, the stream must be byte-for-byte what it was
// before: no extra chunk.
func TestStreamOmitsUsageChunkWhenNotRequested(t *testing.T) {
	rec := runStream(t, false)
	frames, done := streamFrames(t, rec.Body.String())

	if !done {
		t.Fatal("stream did not end with [DONE]")
	}
	for i, f := range frames {
		if _, present := f["usage"]; present {
			t.Errorf("frame %d carries usage but the client did not ask: %v", i, f)
		}
	}
	// Frame count is not asserted: how many chunks the origin's events produce
	// is the converter's business, and pinning it here would fail on an
	// unrelated formatting change. What matters is that no usage rides along.
}

// Criterion: with the option off the stream is unchanged. Pin it structurally --
// same number of chunks, same content, no usage key anywhere, and [DONE] last.
func TestStreamShapeUnchangedWhenUsageNotRequested(t *testing.T) {
	withOff := runStream(t, false).Body.String()

	frames, done := streamFrames(t, withOff)
	if !done {
		t.Fatal("[DONE] missing")
	}
	for i, f := range frames {
		if _, present := f["usage"]; present {
			t.Errorf("frame %d carries usage with the option off: %v", i, f)
		}
		choices, _ := f["choices"].([]any)
		if len(choices) != 1 {
			t.Errorf("frame %d has %d choices, want 1 (no usage chunk expected)", i, len(choices))
		}
	}
	// The delta text must survive verbatim.
	var sawText bool
	for _, f := range frames {
		choices, _ := f["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok && s == "hello" {
			sawText = true
		}
	}
	if !sawText {
		t.Error("the content delta was lost")
	}
}
