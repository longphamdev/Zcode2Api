package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// oaFrame is the shape anthEventToOA hands to the SSE writer.
type oaFrame struct {
	FinishReason string
	ToolCalls    []map[string]any
}

// collectFrames drives anthEventToOA over a synthetic Anthropic event stream
// and returns the OpenAI chunks that carry either tool_calls or finish_reason.
func collectFrames(t *testing.T, sse string) []oaFrame {
	t.Helper()
	st := &oaStreamState{toolIdx: map[int]int{}}
	var out []oaFrame
	err := scanAnthropicSSE(strings.NewReader(sse), func(ev string, data map[string]any) {
		chunk := anthEventToOA(ev, data, "id-1", "glm-5.3", st)
		if chunk == nil {
			return
		}
		var f oaFrame
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			return
		}
		choice, _ := choices[0].(map[string]any)
		if fr, ok := choice["finish_reason"].(string); ok {
			f.FinishReason = fr
		}
		delta, _ := choice["delta"].(map[string]any)
		if tc, ok := delta["tool_calls"].([]any); ok {
			for _, c := range tc {
				if m, ok := c.(map[string]any); ok {
					f.ToolCalls = append(f.ToolCalls, m)
				}
			}
		}
		out = append(out, f)
	})
	if err != nil {
		t.Fatalf("scanAnthropicSSE: %v", err)
	}
	return out
}

// finishReasons returns every non-empty finish_reason emitted, in order.
func finishReasons(frames []oaFrame) []string {
	var out []string
	for _, f := range frames {
		if f.FinishReason != "" {
			out = append(out, f.FinishReason)
		}
	}
	return out
}

// toolCallIndexes returns the tool_calls[].index values seen, in order.
func toolCallIndexes(frames []oaFrame) []int {
	var out []int
	for _, f := range frames {
		for _, tc := range f.ToolCalls {
			if idx, ok := tc["index"].(int); ok {
				out = append(out, idx)
			}
		}
	}
	return out
}

// A stream carrying thinking + text + two parallel tool calls is exactly the
// shape that broke Codex-style clients: tool indexes arrived as 1 and 3
// (Anthropic content-block indexes) and message_stop overrode the tool_calls
// finish_reason with "stop", so the client ended the turn without running the
// tool. See upstream issue #2.
func TestAnthEventToOAToolCallIndexesAndSingleFinishReason(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start` + "\n" + `data: {"type":"message_start"}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_a","name":"run_shell"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}` + "\n\n",
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call_b","name":"read_file"}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a\"}"}}` + "\n\n",
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}, "")

	frames := collectFrames(t, sse)

	// Tool-call indexes must be sequential among tool calls only, starting at 0.
	gotIdx := toolCallIndexes(frames)
	wantIdx := []int{0, 0, 1, 1} // announce(0), args(0), announce(1), args(1)
	if len(gotIdx) != len(wantIdx) {
		t.Fatalf("tool index sequence = %v, want %v", gotIdx, wantIdx)
	}
	for i := range wantIdx {
		if gotIdx[i] != wantIdx[i] {
			t.Fatalf("tool index sequence = %v, want %v", gotIdx, wantIdx)
		}
	}

	// Exactly one finish_reason, and it must be tool_calls — message_stop must
	// not append a second "stop" chunk.
	gotFinish := finishReasons(frames)
	if len(gotFinish) != 1 {
		t.Fatalf("finish_reason sequence = %v, want exactly one entry", gotFinish)
	}
	if gotFinish[0] != "tool_calls" {
		t.Fatalf("finish_reason = %q, want %q", gotFinish[0], "tool_calls")
	}
}

// A text-only turn must still get its single "stop".
func TestAnthEventToOATextOnlyStops(t *testing.T) {
	sse := strings.Join([]string{
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}, "")

	got := finishReasons(collectFrames(t, sse))
	if len(got) != 1 || got[0] != "stop" {
		t.Fatalf("finish_reason sequence = %v, want exactly [stop]", got)
	}
}

// If the truncated-turn case never emits message_delta, message_stop must
// still produce a finish_reason so the client is not left hanging.
func TestAnthEventToOAMessageStopWithoutDeltaStillFinishes(t *testing.T) {
	sse := `event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n"

	got := finishReasons(collectFrames(t, sse))
	if len(got) != 1 || got[0] != "stop" {
		t.Fatalf("finish_reason sequence = %v, want exactly [stop]", got)
	}
}

// An input_json_delta whose content_block_start was lost (dropped SSE line,
// truncated stream) must not be swallowed: the tool call was already announced
// with empty arguments, so discarding the payload makes the client parse "".
func TestAnthEventToOAOrphanInputDeltaKeepsArguments(t *testing.T) {
	sse := `event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":4,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}` + "\n\n"

	frames := collectFrames(t, sse)
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1 (orphan delta must not be dropped)", len(frames))
	}
	if len(frames[0].ToolCalls) != 1 {
		t.Fatalf("got %d tool_calls, want 1", len(frames[0].ToolCalls))
	}
	fn, _ := frames[0].ToolCalls[0]["function"].(map[string]any)
	if got, _ := fn["arguments"].(string); got != `{"a":1}` {
		t.Fatalf("arguments = %q, want %q", got, `{"a":1}`)
	}
}

// The non-stream JSON path used to keep only text blocks and always report
// finish_reason "stop", so a tool-only response reached OpenAI clients with no
// tool_calls at all.
func TestWriteOpenAIFromJSONKeepsToolUse(t *testing.T) {
	body := `{
		"id": "msg_1",
		"type": "message",
		"role": "assistant",
		"model": "glm-5.3",
		"stop_reason": "tool_use",
		"content": [
			{"type": "text", "text": "Let me check."},
			{"type": "tool_use", "id": "call_a", "name": "run_shell", "input": {"cmd": "ls"}},
			{"type": "tool_use", "id": "call_b", "name": "read_file", "input": {"path": "a"}}
		]
	}`

	rec := httptest.NewRecorder()
	writeOpenAIFromJSON(rec, 200, strings.NewReader(body), "req-1", "glm-5.3")

	var out struct {
		Choices []struct {
			Message struct {
				Content   string           `json:"content"`
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, rec.Body.String())
	}
	if len(out.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(out.Choices))
	}
	ch := out.Choices[0]
	if ch.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want %q", ch.FinishReason, "tool_calls")
	}
	if len(ch.Message.ToolCalls) != 2 {
		t.Fatalf("got %d tool_calls, want 2", len(ch.Message.ToolCalls))
	}
	if ch.Message.Content != "Let me check." {
		t.Fatalf("content = %q, want text preserved alongside tools", ch.Message.Content)
	}
	for i, want := range []struct{ id, name, args string }{
		{"call_a", "run_shell", `{"cmd":"ls"}`},
		{"call_b", "read_file", `{"path":"a"}`},
	} {
		tc := ch.Message.ToolCalls[i]
		if got, _ := tc["id"].(string); got != want.id {
			t.Fatalf("tool_calls[%d].id = %q, want %q", i, got, want.id)
		}
		fn, _ := tc["function"].(map[string]any)
		if got, _ := fn["name"].(string); got != want.name {
			t.Fatalf("tool_calls[%d].function.name = %q, want %q", i, got, want.name)
		}
		// Compare parsed JSON: the raw upstream `input` keeps its original
		// whitespace, so a byte comparison would be brittle.
		gotArgs, _ := fn["arguments"].(string)
		var gotObj, wantObj map[string]any
		if err := json.Unmarshal([]byte(gotArgs), &gotObj); err != nil {
			t.Fatalf("tool_calls[%d].function.arguments = %q is not valid JSON: %v", i, gotArgs, err)
		}
		if err := json.Unmarshal([]byte(want.args), &wantObj); err != nil {
			t.Fatalf("bad test expectation: %v", err)
		}
		if !reflect.DeepEqual(gotObj, wantObj) {
			t.Fatalf("tool_calls[%d].function.arguments = %v, want %v", i, gotObj, wantObj)
		}
	}
}

// A tool_use block with no input still needs a parseable arguments string.
func TestWriteOpenAIFromJSONEmptyArgumentsDefaultsToObject(t *testing.T) {
	body := `{"id":"msg_1","role":"assistant","stop_reason":"tool_use",
		"content":[{"type":"tool_use","id":"call_a","name":"ping"}]}`

	rec := httptest.NewRecorder()
	writeOpenAIFromJSON(rec, 200, strings.NewReader(body), "req-1", "glm-5.3")

	if !strings.Contains(rec.Body.String(), `"arguments":"{}"`) {
		t.Fatalf("expected empty arguments to default to {}, body=%s", rec.Body.String())
	}
}

func TestOAFinishReason(t *testing.T) {
	cases := map[string]string{
		"tool_use":        "tool_calls",
		"max_tokens":      "length",
		"end_turn":        "stop",
		"stop_sequence":   "stop",
		"":                "stop",
		"something_weird": "stop",
	}
	for in, want := range cases {
		if got := oaFinishReason(in); got != want {
			t.Errorf("oaFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}
