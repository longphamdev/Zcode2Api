package gateway

import (
	"encoding/json"
	"testing"
)

// chunkChoice decodes one chunk the way a client would and returns its first
// choice, plus whether the finish_reason key was present at all.
func chunkChoice(t *testing.T, chunk map[string]any) (choice map[string]json.RawMessage, present bool) {
	t.Helper()
	raw, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	var wire struct {
		Choices []map[string]json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal chunk: %v", err)
	}
	if len(wire.Choices) == 0 {
		t.Fatalf("chunk has no choices: %s", raw)
	}
	_, present = wire.Choices[0]["finish_reason"]
	return wire.Choices[0], present
}

// The OpenAI schema marks finish_reason required (nullable) on every stream
// choice, so a content delta must carry the key with a JSON null value — not
// omit it, and not use the string "null".
func TestStreamContentChunkCarriesNullFinishReason(t *testing.T) {
	st := &oaStreamState{toolIdx: map[int]int{}}
	chunk := anthEventToOA("content_block_delta",
		map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "hello"}},
		"id-1", "glm-5.3", st)

	if chunk == nil {
		t.Fatal("content delta produced no chunk")
	}
	choice, present := chunkChoice(t, chunk)
	if !present {
		t.Fatal("finish_reason key is absent on a content chunk")
	}

	var v any
	if err := json.Unmarshal(choice["finish_reason"], &v); err != nil {
		t.Fatalf("finish_reason is not valid JSON: %v", err)
	}
	if v != nil {
		t.Errorf("finish_reason = %#v, want JSON null on a non-terminal chunk", v)
	}
}

// A tool-call announcement is also non-terminal: the arguments still follow.
func TestStreamToolCallAnnouncementCarriesNullFinishReason(t *testing.T) {
	st := &oaStreamState{toolIdx: map[int]int{}}
	chunk := anthEventToOA("content_block_start",
		map[string]any{"type": "content_block_start", "index": 2,
			"content_block": map[string]any{"type": "tool_use", "id": "call_a", "name": "run_shell"}},
		"id-1", "glm-5.3", st)

	if chunk == nil {
		t.Fatal("tool_use block produced no chunk")
	}
	if _, present := chunkChoice(t, chunk); !present {
		t.Fatal("finish_reason key is absent on a tool-call chunk")
	}
}

// The terminal chunk carries the real reason, and that reason must be one of
// the values the OpenAI enum allows.
func TestStreamTerminalChunkCarriesRealFinishReason(t *testing.T) {
	allowed := map[string]bool{
		"stop": true, "length": true, "tool_calls": true,
		"content_filter": true, "function_call": true,
	}

	for _, c := range []struct{ stopReason, want string }{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
	} {
		st := &oaStreamState{toolIdx: map[int]int{}}
		chunk := anthEventToOA("message_delta",
			map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": c.stopReason}},
			"id-1", "glm-5.3", st)
		if chunk == nil {
			t.Fatalf("%s produced no chunk", c.stopReason)
		}
		choice, present := chunkChoice(t, chunk)
		if !present {
			t.Fatalf("%s: finish_reason key absent on the terminal chunk", c.stopReason)
		}
		var got string
		if err := json.Unmarshal(choice["finish_reason"], &got); err != nil {
			t.Fatalf("%s: finish_reason is not a string or null: %v", c.stopReason, err)
		}
		if got != c.want {
			t.Errorf("stop_reason %s -> finish_reason %q, want %q", c.stopReason, got, c.want)
		}
		if !allowed[got] {
			t.Errorf("finish_reason %q is outside the OpenAI enum", got)
		}
	}
}

// The single-finish-reason guarantee predates this change and must survive it:
// message_stop must not append a second non-null reason after message_delta.
func TestStreamEmitsExactlyOneNonNilFinishReason(t *testing.T) {
	st := &oaStreamState{toolIdx: map[int]int{}}
	events := []struct {
		ev   string
		data map[string]any
	}{
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "hi"}}},
		{"message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"}}},
		{"message_stop", map[string]any{"type": "message_stop"}},
	}

	nonNil := 0
	for _, e := range events {
		chunk := anthEventToOA(e.ev, e.data, "id-1", "glm-5.3", st)
		if chunk == nil {
			continue
		}
		choice, present := chunkChoice(t, chunk)
		if !present {
			t.Fatalf("%s: finish_reason key absent", e.ev)
		}
		var v any
		if err := json.Unmarshal(choice["finish_reason"], &v); err != nil {
			t.Fatalf("%s: %v", e.ev, err)
		}
		if v != nil {
			nonNil++
		}
	}
	if nonNil != 1 {
		t.Errorf("got %d chunks with a non-null finish_reason, want exactly 1", nonNil)
	}
}
