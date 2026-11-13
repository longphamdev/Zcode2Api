package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The ticket's worked example: 15 uncached + 1344 cached prompt tokens, 100 out.
// Anthropic's input_tokens excludes the cached tokens, so OpenAI's
// prompt_tokens — "tokens in the prompt" — has to sum them.
func TestOAUsageSumsCachedIntoPromptTokens(t *testing.T) {
	got := oaUsage(Usage{In: 15, Out: 100, CacheRead: 1344, CacheCreate: 0})

	if got["prompt_tokens"] != 1359 {
		t.Errorf("prompt_tokens = %v, want 1359 (15 + 1344 + 0)", got["prompt_tokens"])
	}
	if got["completion_tokens"] != 100 {
		t.Errorf("completion_tokens = %v, want 100", got["completion_tokens"])
	}
	if got["total_tokens"] != 1459 {
		t.Errorf("total_tokens = %v, want 1459 (prompt + completion, cache counted once)", got["total_tokens"])
	}
}

// Cache creation tokens are prompt tokens too — they were written to the cache
// on this very request, so they belong in the prompt count.
func TestOAUsageCountsCacheCreation(t *testing.T) {
	got := oaUsage(Usage{In: 10, Out: 5, CacheRead: 0, CacheCreate: 200})

	if got["prompt_tokens"] != 210 {
		t.Errorf("prompt_tokens = %v, want 210 (10 + 0 + 200)", got["prompt_tokens"])
	}
	if got["total_tokens"] != 215 {
		t.Errorf("total_tokens = %v, want 215", got["total_tokens"])
	}
}

func TestOAUsageReportsCachedTokensInDetails(t *testing.T) {
	got := oaUsage(Usage{In: 15, Out: 100, CacheRead: 1344, CacheCreate: 0})

	details, ok := got["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_tokens_details missing or not an object: %T", got["prompt_tokens_details"])
	}
	if details["cached_tokens"] != 1344 {
		t.Errorf("prompt_tokens_details.cached_tokens = %v, want 1344", details["cached_tokens"])
	}
}

// There is no trustworthy source for reasoning_tokens, so the object is omitted
// entirely rather than populated with an invented zero.
func TestOAUsageOmitsCompletionTokenDetails(t *testing.T) {
	got := oaUsage(Usage{In: 15, Out: 100, CacheRead: 1344})

	if _, present := got["completion_tokens_details"]; present {
		t.Errorf("completion_tokens_details must not be emitted; got %v", got["completion_tokens_details"])
	}
}

// A request that used no cache still gets the details object with a zero, so
// clients reading the field do not have to special-case its absence.
func TestOAUsageZeroCacheStillReportsDetails(t *testing.T) {
	got := oaUsage(Usage{In: 20, Out: 7})

	details, ok := got["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_tokens_details missing: %v", got["prompt_tokens_details"])
	}
	if details["cached_tokens"] != 0 {
		t.Errorf("cached_tokens = %v, want 0", details["cached_tokens"])
	}
	if got["prompt_tokens"] != 20 {
		t.Errorf("prompt_tokens = %v, want 20", got["prompt_tokens"])
	}
}

// decodeBody unmarshals an httptest recorder body into the shape an OpenAI
// client sees.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func usageFrom(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	u, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatalf("usage missing or not an object: %T (body: %v)", out["usage"], out)
	}
	return u
}

// The plain-JSON branch: the origin answered with a complete Anthropic message
// document. It already carries the token triple, and the scanner has seen those
// bytes by the time the body has been read.
func TestWriteOpenAIFromJSONEmitsUsage(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","stop_reason":"end_turn",` +
		`"content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":15,"output_tokens":100,"cache_read_input_tokens":1344}}`

	// Mirror the production topology: forward.go tees the origin body into the
	// scanner before handing it to the writer, so the writer reads a reader
	// that has already been scanned.
	var scn usageScanner
	src := io.NopCloser(io.TeeReader(strings.NewReader(body), &scn))

	rec := httptest.NewRecorder()
	writeOpenAIFromJSON(rec, 200, src, "req-1", "glm-5.3", &scn)

	u := usageFrom(t, decodeBody(t, rec))
	// JSON unmarshals numbers into float64; these are whole tokens.
	if got := u["prompt_tokens"]; got != float64(1359) {
		t.Errorf("prompt_tokens = %v, want 1359", got)
	}
	if got := u["completion_tokens"]; got != float64(100) {
		t.Errorf("completion_tokens = %v, want 100", got)
	}
	if got := u["total_tokens"]; got != float64(1459) {
		t.Errorf("total_tokens = %v, want 1459", got)
	}
	details, ok := u["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_tokens_details missing: %v", u["prompt_tokens_details"])
	}
	if got := details["cached_tokens"]; got != float64(1344) {
		t.Errorf("cached_tokens = %v, want 1344", got)
	}
	if _, present := u["completion_tokens_details"]; present {
		t.Errorf("completion_tokens_details must not be emitted")
	}
}

// The other non-stream branch: the client asked for no stream, but the origin
// answered with SSE anyway, so the converter collects the events into a single
// completion. That path has to carry usage too.
func TestWriteOpenAIFromOriginSSECollectedEmitsUsage(t *testing.T) {
	sse := usageStreamSSE()

	head := []byte("event: message_start\n")
	// Production tees the origin body (head included) into the scanner; the
	// writer then streams from that same reader.
	var scn usageScanner
	tee := io.NopCloser(io.TeeReader(strings.NewReader(sse), &scn))
	resp := &http.Response{StatusCode: 200, Body: tee}

	rec := httptest.NewRecorder()
	srv := &Server{}
	srv.writeOpenAIFromOrigin(rec, resp, head, tee, "glm-5.3", false, &scn, false)

	out := decodeBody(t, rec)
	u := usageFrom(t, out)
	if got := u["prompt_tokens"]; got != float64(1359) {
		t.Errorf("prompt_tokens = %v, want 1359", got)
	}
	if got := u["completion_tokens"]; got != float64(100) {
		t.Errorf("completion_tokens = %v, want 100", got)
	}
	if got := u["total_tokens"]; got != float64(1459) {
		t.Errorf("total_tokens = %v, want 1459", got)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(choices))
	}
}

// Nothing measured means no usage key at all: an all-zero object would claim a
// measurement of zero, which is the mirror of the reasoning-tokens rule.
func TestOAUsageReturnsNilWhenNothingMeasured(t *testing.T) {
	if got := oaUsage(Usage{}); got != nil {
		t.Errorf("oaUsage(zero) = %v, want nil", got)
	}
}

func TestWriteOpenAIFromJSONOmitsUsageWhenUnmeasured(t *testing.T) {
	// An origin document with no usage field: nothing to report.
	body := `{"id":"msg_1","type":"message","role":"assistant","stop_reason":"end_turn",` +
		`"content":[{"type":"text","text":"hi"}]}`

	var scn usageScanner
	src := io.NopCloser(io.TeeReader(strings.NewReader(body), &scn))

	rec := httptest.NewRecorder()
	writeOpenAIFromJSON(rec, 200, src, "req-1", "glm-5.3", &scn)

	out := decodeBody(t, rec)
	if _, present := out["usage"]; present {
		t.Errorf("usage must be omitted when nothing was measured; got %v", out["usage"])
	}
}
