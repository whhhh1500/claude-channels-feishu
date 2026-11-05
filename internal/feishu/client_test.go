package feishu

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	lark "github.com/larksuite/oapi-sdk-go/v3"
)

func TestReplyCannotCrossChats(t *testing.T) {
	var replies atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			fmt.Fprint(w, `{"code":0,"tenant_access_token":"offline-test","expire":7200}`)
		case "/open-apis/im/v1/messages/om_target":
			fmt.Fprint(w, `{"code":0,"data":{"items":[{"message_id":"om_target","chat_id":"oc_actual"}]}}`)
		case "/open-apis/im/v1/messages/om_target/reply":
			replies.Add(1)
			fmt.Fprint(w, `{"code":0,"data":{"message_id":"om_reply"}}`)
		default:
			t.Errorf("unexpected API request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	f := &Client{cli: lark.NewClient("offline-app", "offline-secret", lark.WithOpenBaseUrl(srv.URL)),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, err := f.SendText(context.Background(), "oc_other", "om_target", "test"); err == nil {
		t.Fatal("cross-chat reply was accepted")
	}
	if replies.Load() != 0 {
		t.Fatal("sent a reply outside the authorized chat")
	}
	if id, err := f.SendText(context.Background(), "oc_actual", "om_target", "test"); err != nil || id != "om_reply" {
		t.Fatalf("valid reply failed: %q %v", id, err)
	}
	if replies.Load() != 1 {
		t.Fatalf("expected one authorized reply, got %d", replies.Load())
	}
}
