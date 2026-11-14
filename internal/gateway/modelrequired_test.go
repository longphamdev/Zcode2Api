package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
