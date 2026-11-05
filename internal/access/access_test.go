package access

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestGate(t *testing.T) {
	no := false
	a := &Access{DMPolicy: "pairing", AllowFrom: []string{"ou_me"}, Groups: map[string]*GroupPolicy{
		"oc_g1": {}, "oc_g2": {RequireMention: &no},
	}}
	cases := []struct {
		sender, chat, typ string
		mention           bool
		want              Result
	}{
		{"ou_me", "oc_p", "p2p", false, Allow},
		{"ou_x", "oc_p", "p2p", false, Pair},
		{"ou_me", "oc_g1", "group", false, Drop},
		{"ou_me", "oc_g1", "group", true, Allow},
		{"ou_x", "oc_g1", "group", true, Drop},
		{"ou_me", "oc_g2", "group", false, Allow},
		{"ou_me", "oc_unknown", "group", true, Drop},
	}
	for _, c := range cases {
		if got := a.Gate(c.sender, c.chat, c.typ, c.mention); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
	a.DMPolicy = "allowlist"
	if a.Gate("ou_x", "oc_p", "p2p", false) != Drop {
		t.Error("allowlist should drop strangers")
	}
	a.DMPolicy = "disabled"
	if a.Gate("ou_me", "oc_p", "p2p", false) != Drop {
		t.Error("disabled must drop allowed users too")
	}
	a.Groups["oc_null"] = nil
	if a.Gate("ou_me", "oc_null", "group", true) != Drop {
		t.Error("null group must not allow access")
	}
}

func TestConcurrentStores(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			err := NewStore(dir).Update(func(a *Access) error {
				a.AllowFrom = append(a.AllowFrom, fmt.Sprintf("ou_%d", i))
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	a, err := NewStore(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if !slices.Contains(a.AllowFrom, fmt.Sprintf("ou_%d", i)) {
			t.Errorf("lost update %d", i)
		}
	}
}

func TestPairing(t *testing.T) {
	s := NewStore(t.TempDir())
	code, resend, err := s.StartPairing("ou_x", "oc_p", "")
	if err != nil || resend || len(code) != 6 {
		t.Fatalf("start: %q %v %v", code, resend, err)
	}
	code2, resend, _ := s.StartPairing("ou_x", "oc_p", "")
	if code2 != code || !resend {
		t.Error("second request should reuse code")
	}
	if _, err := s.ApprovePairing("zzzzzz"); err == nil {
		t.Error("bad code should fail")
	}
	if _, err := s.ApprovePairing(strings.ToUpper(code)); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Load()
	if len(a.AllowFrom) != 1 || a.AllowFrom[0] != "ou_x" || len(a.Pending) != 0 {
		t.Errorf("access = %+v", a)
	}
}
