package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"zcode2api/internal/upstream"
	"zcode2api/internal/util"
)

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 50<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The OpenAI-only request options have to be read from the client's body:
	// the Anthropic wire format has no place for them, so the conversion below
	// drops them.
	includeUsage := ParseIncludeUsage(raw)
	anth, err := OpenAIToAnthropic(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.routeAnthropic(w, r, anth, true, includeUsage)
}

func OpenAIToAnthropic(raw []byte) ([]byte, error) {
	var req struct {
		Model            string          `json:"model"`
		Stream           bool            `json:"stream"`
		MaxTokens        int             `json:"max_tokens"`
		MaxCompletionTok int             `json:"max_completion_tokens"`
		Stop             []string        `json:"stop"`
		ToolChoice       json.RawMessage `json:"tool_choice"`
		Messages         []oaMessage     `json:"messages"`
		Tools            []oaTool        `json:"tools"`
		Thinking         json.RawMessage `json:"thinking"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":  req.Model,
		"stream": req.Stream,
	}
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = req.MaxCompletionTok // OpenAI's newer field
	}
	if maxTok > 0 {
		out["max_tokens"] = maxTok
	} else {
		out["max_tokens"] = 4096 // OpenAI-compat default
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}
	if len(req.Thinking) > 0 && string(req.Thinking) != "null" {
		var t any
		_ = json.Unmarshal(req.Thinking, &t)
		out["thinking"] = t
	}
	var system []any
	var msgs []any
	for _, m := range req.Messages {
		if m.Role == "system" || m.Role == "developer" {
			system = append(system, map[string]any{"type": "text", "text": m.Text()})
			continue
		}
		msgs = append(msgs, openaiMsgToAnth(m))
	}
	if len(system) > 0 {
		out["system"] = system
	}
	out["messages"] = msgs
	if len(req.Tools) > 0 {
		var tools []any
		for _, t := range req.Tools {
			schema := t.Function.Parameters
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": schema,
			})
		}
		// tool_choice: none drops tools entirely; required/named map through
		choice := "auto"
		var tcName string
		if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
			var tc any
			if json.Unmarshal(req.ToolChoice, &tc) == nil {
				switch v := tc.(type) {
				case string:
					if v == "none" {
						tools = nil
					} else if v == "required" {
						choice = "any"
					}
				case map[string]any:
					if fn, ok := v["function"].(map[string]any); ok {
						if n, ok := fn["name"].(string); ok {
							choice = "tool"
							tcName = n
						}
					}
				}
			}
		}
		if len(tools) > 0 {
			out["tools"] = tools
			if choice == "tool" {
				out["tool_choice"] = map[string]any{"type": "tool", "name": tcName}
			} else {
				out["tool_choice"] = map[string]any{"type": choice}
			}
		}
	}
	return json.Marshal(out)
}

type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

func (m oaMessage) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	if m.Content[0] == '"' {
		var s string
		_ = json.Unmarshal(m.Content, &s)
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(m.Content)
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func openaiMsgToAnth(m oaMessage) map[string]any {
	if m.Role == "tool" {
		return map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Text(),
			}},
		}
	}
	if m.Role == "assistant" && len(m.ToolCalls) > 0 {
		var blocks []any
		if t := m.Text(); t != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": t})
		}
		for _, tc := range m.ToolCalls {
			var input any
			if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
				input = map[string]any{}
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
		return map[string]any{"role": "assistant", "content": blocks}
	}
	// multimodal content: text parts + image_url data-URLs → image blocks
	if blocks := m.contentBlocks(); blocks != nil {
		return map[string]any{"role": m.Role, "content": blocks}
	}
	return map[string]any{"role": m.Role, "content": m.Text()}
}

// contentBlocks converts OpenAI multimodal parts to Anthropic blocks.
// Data-URL images become base64 image blocks; remote URLs are skipped
// Nil = plain string content.
func (m oaMessage) contentBlocks() []any {
	if len(m.Content) == 0 || m.Content[0] != '[' {
		return nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return nil
	}
	var blocks []any
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			}
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			if media, data, ok := strings.Cut(p.ImageURL.URL, ","); ok &&
				strings.HasPrefix(media, "data:") {
				fmt_ := strings.TrimPrefix(media, "data:")
				if i := strings.Index(fmt_, ";"); i >= 0 {
					fmt_ = fmt_[:i]
				}
				blocks = append(blocks, map[string]any{
					"type": "image", "source": map[string]any{
						"type": "base64", "media_type": fmt_, "data": data,
					},
				})
			}
		}
	}
	if len(blocks) == 0 {
		return nil
	}
	return blocks
}

// newOAChunk builds a stream chunk envelope. The same five keys appear on every
// chunk; only the choices and the optional usage differ.
func newOAChunk(id, model string, choices []any, usage map[string]any) map[string]any {
	if choices == nil {
		choices = []any{}
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": choices,
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return chunk
}

// writeSSEChunk marshals one chunk and writes it as an SSE frame, flushing so
// the client sees it immediately.
func writeSSEChunk(w http.ResponseWriter, fl http.Flusher, chunk map[string]any) {
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if fl != nil {
		fl.Flush()
	}
}

func (s *Server) writeOpenAIFromOrigin(w http.ResponseWriter, resp *http.Response, head []byte, rest io.ReadCloser, outModel string, stream bool, scn *usageScanner, includeUsage bool) {
	defer resp.Body.Close()
	id := "chatcmpl-" + util.GenerateUUID()
	// rest (the wrapped resp.Body) already starts with head — head feeds
	// detection only, never the byte stream
	if !upstream.IsSSE(head) {
		// non-stream upstream: a complete Anthropic message JSON — convert it
		writeOpenAIFromJSON(w, resp.StatusCode, rest, id, outModel, scn)
		return
	}
	src := rest
	if !stream {
		text, tools, err := collectAnthropic(src)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, err.Error())
			return
		}
		msg := map[string]any{"role": "assistant", "content": text}
		finish := "stop"
		if len(tools) > 0 {
			msg["tool_calls"] = tools
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(withUsage(map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   outModel,
			"choices": []any{map[string]any{
				"index": 0, "message": msg, "finish_reason": finish,
			}},
		}, scn.Usage()))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	st := &oaStreamState{toolIdx: map[int]int{}}
	err := scanAnthropicSSE(src, func(ev string, data map[string]any) {
		if chunk := anthEventToOA(ev, data, id, outModel, st); chunk != nil {
			writeSSEChunk(w, fl, chunk)
		}
	})
	if err != nil {
		return
	}
	// The scanner has now seen the whole body, message_delta included, so the
	// token triple is complete. A client that asked for it gets one terminal
	// chunk with an empty choices array — the shape OpenAI uses — and then the
	// [DONE] sentinel. Clients that did not ask see exactly what they saw
	// before.
	if includeUsage {
		if usage := oaUsage(scn.Usage()); usage != nil {
			writeSSEChunk(w, fl, newOAChunk(id, outModel, nil, usage))
		}
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// oaUsage renders a scanned Anthropic usage triple as the OpenAI `usage`
// object. Anthropic reports input_tokens with the cached prompt tokens
// excluded, while OpenAI's prompt_tokens means every token in the prompt, so
// the cache counters are summed in — once, not again in total_tokens.
//
// completion_tokens_details is deliberately absent: the origin reports no
// reasoning-token count, and a zero there would read as "reasoning measured,
// none used" rather than "not measured".
//
// Returns nil when nothing was measured, for the same reason: an all-zero
// object claims a measurement of zero. The caller omits the key instead.
func oaUsage(u Usage) map[string]any {
	if u.In == 0 && u.Out == 0 && u.CacheRead == 0 && u.CacheCreate == 0 {
		return nil
	}
	prompt := u.In + u.CacheRead + u.CacheCreate
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": u.Out,
		"total_tokens":      prompt + u.Out,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": u.CacheRead,
		},
	}
}

// withUsage attaches the decoded usage object to an encoded completion, or
// leaves the key out entirely when there was nothing to report. Callers build
// their envelope as a map; this keeps the omit-empty rule in one place so the
// two non-stream branches cannot disagree about it.
func withUsage(body map[string]any, u Usage) map[string]any {
	if usage := oaUsage(u); usage != nil {
		body["usage"] = usage
	}
	return body
}

// writeOpenAIFromJSON converts a complete Anthropic message document (the
// origin's non-stream reply) into an OpenAI chat.completion.
func writeOpenAIFromJSON(w http.ResponseWriter, status int, body io.Reader, id, outModel string, scn *usageScanner) {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "read origin response: "+err.Error())
		return
	}
	var doc struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		writeOpenAIError(w, status, string(raw))
		return
	}
	text := strings.Builder{}
	var tools []any
	for _, b := range doc.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			args := string(b.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			tools = append(tools, map[string]any{
				"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": args},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": text.String()}
	finish := oaFinishReason(doc.StopReason)
	if len(tools) > 0 {
		msg["tool_calls"] = tools
		finish = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(withUsage(map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   outModel,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
	}, scn.Usage()))
}

func collectAnthropic(r io.Reader) (string, []any, error) {
	var text strings.Builder
	var tools []any
	var curTool map[string]any
	var args strings.Builder
	err := scanAnthropicSSE(r, func(ev string, data map[string]any) {
		switch ev {
		case "content_block_start":
			cb, _ := data["content_block"].(map[string]any)
			if upstream.Str(cb["type"]) == "tool_use" {
				curTool = map[string]any{
					"id": upstream.Str(cb["id"]), "type": "function",
					"function": map[string]any{"name": upstream.Str(cb["name"]), "arguments": ""},
				}
				args.Reset()
			}
		case "content_block_delta":
			d, _ := data["delta"].(map[string]any)
			switch upstream.Str(d["type"]) {
			case "text_delta":
				text.WriteString(upstream.Str(d["text"]))
			case "input_json_delta":
				args.WriteString(upstream.Str(d["partial_json"]))
			}
		case "content_block_stop":
			if curTool != nil {
				fn, _ := curTool["function"].(map[string]any)
				fn["arguments"] = args.String()
				tools = append(tools, curTool)
				curTool = nil
			}
		}
	})
	return text.String(), tools, err
}

func scanAnthropicSSE(r io.Reader, fn func(event string, data map[string]any)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	var ev string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event:") {
			ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var data map[string]any
		if json.Unmarshal([]byte(payload), &data) != nil {
			continue
		}
		if t := upstream.Str(data["type"]); t != "" {
			ev = t
		}
		fn(ev, data)
	}
	return sc.Err()
}

// oaStreamState tracks one converted stream. OpenAI numbers tool calls
// 0,1,2… among tool calls only, while Anthropic's index counts every content
// block (thinking and text included), so the two must be mapped. finished
// records that finish_reason was already sent so message_stop doesn't
// overwrite "tool_calls" with "stop".
type oaStreamState struct {
	toolIdx  map[int]int
	finished bool
}

func oaFinishReason(stopReason string) string {
	switch stopReason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	}
	return "stop"
}

func anthEventToOA(ev string, data map[string]any, id, model string, st *oaStreamState) map[string]any {
	delta := map[string]any{}
	var finish *string
	switch ev {
	case "content_block_delta":
		d, _ := data["delta"].(map[string]any)
		switch upstream.Str(d["type"]) {
		case "text_delta":
			delta["content"] = upstream.Str(d["text"])
		case "thinking_delta":
			delta["reasoning_content"] = upstream.Str(d["thinking"])
		case "input_json_delta":
			// A delta whose content_block_start went missing (a dropped or
			// truncated SSE line) must not silently discard the arguments: the
			// tool call was already announced with `arguments: ""`, so dropping
			// the payload leaves the client parsing an empty string. Fall back to
			// the block index itself, then allocate a slot for it so any further
			// deltas for the same block agree.
			upIdx := upstream.AsInt(data["index"])
			idx, ok := st.toolIdx[upIdx]
			if !ok {
				idx = len(st.toolIdx)
				st.toolIdx[upIdx] = idx
				log.Printf("[openai] tool delta for unseen content block %d — allocating tool index %d", upIdx, idx)
			}
			delta["tool_calls"] = []any{map[string]any{
				"index": idx, "function": map[string]any{"arguments": upstream.Str(d["partial_json"])},
			}}
		default:
			return nil
		}
	case "content_block_start":
		cb, _ := data["content_block"].(map[string]any)
		if upstream.Str(cb["type"]) != "tool_use" {
			return nil
		}
		idx := len(st.toolIdx)
		st.toolIdx[upstream.AsInt(data["index"])] = idx
		delta["tool_calls"] = []any{map[string]any{
			"index": idx, "id": upstream.Str(cb["id"]), "type": "function",
			"function": map[string]any{"name": upstream.Str(cb["name"]), "arguments": ""},
		}}
	case "message_delta":
		md, _ := data["delta"].(map[string]any)
		if sr := upstream.Str(md["stop_reason"]); sr != "" {
			// Only the first stop_reason wins. The origin can send several
			// message_delta events (only the terminal one normally carries a
			// reason), and a repeat must not emit a second finish_reason.
			if st.finished {
				return nil
			}
			fr := oaFinishReason(sr)
			finish = &fr
			st.finished = true
		}
	case "message_stop":
		if st.finished {
			return nil
		}
		fr := "stop"
		finish = &fr
		st.finished = true
	default:
		return nil
	}
	// The OpenAI schema marks finish_reason required (nullable) on every stream
	// choice, so it is always present: null while the turn is still running, the
	// real reason on the terminal chunk. Omitting it on content chunks is what
	// strict clients reject.
	ch := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != nil {
		ch["finish_reason"] = *finish
	}
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": model, "choices": []any{ch},
	}
}

func writeOpenAIError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error"},
	})
}

func writeAnthropicError(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error", "error": map[string]any{"type": typ, "message": msg},
	})
}
