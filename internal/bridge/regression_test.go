package bridge

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/whhhh1500/claude-channels-feishu/internal/config"
	"github.com/whhhh1500/claude-channels-feishu/internal/session"
)

func TestDedupExpiresBelowCleanupThreshold(t *testing.T) {
	b, _ := testBridge(t)
	b.seen["msg:old"] = time.Now().Add(-dedupTTL - time.Second)
	if !b.markSeen("msg:old") || b.markSeen("msg:old") {
		t.Fatal("expired ID must be accepted exactly once")
	}
}

func TestConnectorOldRunCannotCloseNewDone(t *testing.T) {
	b, _ := testBridge(t)
	oldDone, newDone := make(chan struct{}), make(chan struct{})
	b.conn.done = newDone
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	go b.conn.run(ctx, &config.Config{}, nil, make(chan struct{}, 1), oldDone)
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("old run did not stop")
	}
	select {
	case <-newDone:
		t.Fatal("old run closed the new generation")
	default:
	}
}

func TestHookCompletionIsAtomic(t *testing.T) {
	b, _ := testBridge(t)
	req := &hookRequest{kind: "permission", result: make(chan string, 1)}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() { b.finishHook(req, "finished", "decision") })
	}
	wg.Wait()
	if !req.done || len(req.result) != 1 || <-req.result != "decision" {
		t.Fatal("completion must publish exactly one result")
	}
}

func TestInvalidQuestionInput(t *testing.T) {
	for _, raw := range []string{"null", "{}", "[]", "broken"} {
		r := &hookRequest{kind: "question", in: hookInput{ToolInput: json.RawMessage(raw)}}
		if _, _, err := hookAnswer(r, "submit", nil); err == nil {
			t.Errorf("accepted invalid input %s", raw)
		}
	}
}

func TestControlCloseClosesMonitor(t *testing.T) {
	b, _ := testBridge(t)
	c, err := StartControl(b, b.log)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	conn, rd, err := session.Dial(&c.info, "subscribe", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := rd.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := rd.ReadString('\n'); err == nil {
		t.Fatal("subscription survived shutdown")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("shutdown did not close subscription")
	}
	if _, err := os.Stat(c.file); !os.IsNotExist(err) {
		t.Fatalf("registration still exists: %v", err)
	}
}
