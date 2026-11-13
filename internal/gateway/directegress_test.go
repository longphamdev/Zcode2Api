package gateway

import (
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/store"
)

func newDirectServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Server{store: db, accLine: map[string]*url.URL{}, hotLine: map[string]time.Time{}}
}

// The bug: choosing "Direct (no proxy)" in the dashboard only cleared the
// profile binding, so profileForAccountMid fell through to the persisted
// "line:<mid>" binding and then to a random pool line. The operator asked for
// direct and got a proxy.
func TestDirectAccountGetsNoLineEvenWithAPersistedBinding(t *testing.T) {
	s := newDirectServer(t)
	acc, err := s.store.Add("jwt.a", "a")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	line, _ := url.Parse("socks5://u:p@192.0.2.10:1104")
	s.accLine[acc.DeviceMid] = line
	s.store.SetSetting("line:"+acc.DeviceMid, line.String())

	if got := s.profileForAccountMid(acc.DeviceMid); got == nil {
		t.Fatal("precondition: the account starts with a line bound")
	}

	s.setAccountDirect(acc.ID, true)

	if got := s.profileForAccountMid(acc.DeviceMid); got != nil {
		t.Fatalf("profileForAccountMid = %s, want nil for a direct account", got)
	}
	if !s.directForAccountMid(acc.DeviceMid) {
		t.Fatal("directForAccountMid = false after choosing direct")
	}
}

// Switching back to pool must restore the pool path.
func TestSwitchingBackToPoolClearsDirect(t *testing.T) {
	s := newDirectServer(t)
	acc, _ := s.store.Add("jwt.a", "a")
	s.setAccountDirect(acc.ID, true)
	s.setAccountDirect(acc.ID, false)

	if s.directForAccountMid(acc.DeviceMid) {
		t.Fatal("still direct after choosing pool")
	}
}

// A dead line must not be rebound onto an account that chose direct.
func TestRebindDoesNotTouchADirectAccount(t *testing.T) {
	s := newDirectServer(t)
	acc, _ := s.store.Add("jwt.a", "a")
	s.setAccountDirect(acc.ID, true)
	dead, _ := url.Parse("socks5://u:p@192.0.2.10:1104")

	s.rebindAccountLine(acc.DeviceMid, dead)

	if _, ok := s.accLine[acc.DeviceMid]; ok {
		t.Fatal("rebindAccountLine bound a line onto a direct account")
	}
	if got := s.store.Setting("line:"+acc.DeviceMid, "unset"); got != "unset" {
		t.Fatalf("line: setting written for a direct account: %q", got)
	}
}
