package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestRequestsWithoutIDDoNotExecute(t *testing.T) {
	var out bytes.Buffer
	s := &Server{CallTool: func(string, json.RawMessage) Result { panic("unreachable") }}
	input := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"reply"}}` + "\n" +
		`{"jsonrpc":"2.0","method":"ping"}` + "\n" +
		`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	if err := s.Serve(strings.NewReader(input), &out, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), `"id":1`) {
		t.Fatalf("unexpected responses: %s", out.String())
	}
}

func TestOutputInitializationConcurrentWithNotifications(t *testing.T) {
	s := &Server{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s.SetOutput(io.Discard, log)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 100; i++ {
			s.SetOutput(io.Discard, log)
		}
	})
	wg.Go(func() {
		for i := 0; i < 100; i++ {
			s.Notify("test", map[string]string{"value": "x"})
		}
	})
	wg.Wait()
}
