package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zcode2api/internal/store"
)

// A request with no model is a client bug. Answering it with the server's
// default model hides that: the caller believes it asked for something it did
// not. The OpenAI spec and llm-comply's chat-error-handling test both expect
// 400/422 for it.
func TestRequestWithoutModelIs400(t *testing.T) {
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &Server{store: db}

	body := []byte(`{"messages":[{"role":"user","content":"test"}],"max_tokens":10}`)
	got := s.injectDefaults(body)

	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("injectDefaults produced invalid JSON: %v", err)
	}
	if _, has := obj["model"]; has {
		t.Fatalf("injectDefaults invented a model: %v", obj["model"])
	}
}

// And the route refuses it, rather than sliding into the default.
func TestRouteAnthropicRefusesMissingModel(t *testing.T) {
	db, _ := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	t.Cleanup(func() { _ = db.Close() })
	s := &Server{store: db}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		bytes.NewReader([]byte(`{"messages":[{"role":"user","content":"test"}]}`)))
	rec := httptest.NewRecorder()
	s.routeAnthropic(rec, req, []byte(`{"messages":[{"role":"user","content":"test"}]}`), false, false)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a request with no model", rec.Code)
	}
}

// The 400 must be in the shape the caller's API expects. An OpenAI client reads
// error.error.code and error.error.param (the bundled spec marks all four
// members required); an Anthropic client reads error.type/message.
func TestMissingModelUsesTheCallerEnvelope(t *testing.T) {
	db, _ := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	t.Cleanup(func() { _ = db.Close() })
	s := &Server{store: db}
	body := []byte(`{"messages":[{"role":"user","content":"test"}]}`)

	t.Run("openai", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.routeAnthropic(rec, req, body, true, false)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		var got struct {
			Error struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Param   *string `json:"param"`
				Code    any     `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body)
		}
		if got.Error.Param == nil || *got.Error.Param != "model" {
			t.Fatalf("error.param = %v, want \"model\"", got.Error.Param)
		}
		if got.Error.Type != "invalid_request_error" {
			t.Fatalf("error.type = %q", got.Error.Type)
		}
		if _, has := rec.Body.String(), true; !has {
			t.Fatal("unreachable")
		}
		if !strings.Contains(rec.Body.String(), `"code":`) {
			t.Fatalf("error.code key missing — the spec marks it required: %s", rec.Body)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.routeAnthropic(rec, req, body, false, false)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		var got map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got["type"] != "error" {
			t.Fatalf("anthropic envelope shape changed: %s", rec.Body)
		}
	})
}
