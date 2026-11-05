package panel

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestPanelAuth(t *testing.T) {
	t.Setenv("FEISHU_STATE_DIR", t.TempDir())
	h := newHandler("127.0.0.1:47830", "secret-token", slog.New(slog.NewTextHandler(io.Discard, nil)))
	do := func(host, token string) int {
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = host
		if token != "" {
			req.Header.Set("X-Panel-Token", token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	if c := do("127.0.0.1:47830", ""); c != 403 {
		t.Errorf("no token: %d", c)
	}
	if c := do("evil.com:47830", "secret-token"); c != 403 {
		t.Errorf("bad host: %d", c)
	}
	if c := do("localhost:47830", "secret-token"); c != 200 {
		t.Errorf("good: %d", c)
	}
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "飞书接入面板") {
		t.Errorf("index: %d", w.Code)
	}
}

func TestConcurrentPanelToken(t *testing.T) {
	dir := t.TempDir()
	tokens := make(chan string, 20)
	var wg sync.WaitGroup
	for i := 0; i < cap(tokens); i++ {
		wg.Go(func() {
			token, err := panelToken(dir)
			if err != nil {
				t.Error(err)
			}
			tokens <- token
		})
	}
	wg.Wait()
	close(tokens)
	first := <-tokens
	for token := range tokens {
		if token != first || len(token) < 16 {
			t.Fatal("concurrent startup replaced the panel token")
		}
	}
}

func TestEmptyPanelTokenRejected(t *testing.T) {
	h := newHandler("127.0.0.1:47830", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := httptest.NewRequest("GET", "http://127.0.0.1:47830/api/state", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("empty token accepted: %d", w.Code)
	}
}
