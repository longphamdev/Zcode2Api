package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/store"
)

func newHoldServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Server{store: db}
}

// An automatic win and a manual win must arm the same hold: the tick must not
// keep polling an Account the operator just claimed by hand.
func TestArmClaimHoldIsIdempotentAcrossCallers(t *testing.T) {
	s := newHoldServer(t)
	endsAt := time.Now().Add(48 * time.Hour).Unix()

	s.armClaimHold(1, endsAt)

	got := s.claimHeldUntil(1)
	if got <= time.Now().Unix() {
		t.Fatalf("hold = %d, want a future horizon", got)
	}
	if got > endsAt {
		t.Fatalf("hold = %d exceeded the offered ends_at %d", got, endsAt)
	}
}

// The horizon is capped at a day, so one far-future ends_at cannot switch the
// claim tick off for a whole plan term with no trace.
func TestArmClaimHoldCapsAtADay(t *testing.T) {
	s := newHoldServer(t)
	now := time.Now().Unix()

	s.armClaimHold(1, now+10*365*24*3600)

	got := s.claimHeldUntil(1)
	if max := now + 86400 + 5; got > max {
		t.Fatalf("hold = %d, want it capped near %d", got, max)
	}
	if got <= now {
		t.Fatalf("hold = %d, want the cap to still be in the future", got)
	}
}

// A zero or negative horizon CLEARS the stored one.
//
// Found by review. An earlier success arms the hold; a later pass that reports
// an unfinished offer returns 0. Treating 0 as "nothing to do" would leave the
// old hold armed, so the failed offer stays suppressed until the cap expires —
// the passage of an unfinished offer must be able to take the silence back.
func TestArmClaimHoldClearsOnZeroAndNegative(t *testing.T) {
	s := newHoldServer(t)
	for _, v := range []int64{0, -1, -1794600000} {
		s.armClaimHold(1, time.Now().Add(time.Hour).Unix())
		if s.claimHeldUntil(1) <= time.Now().Unix() {
			t.Fatalf("precondition failed: hold was not armed")
		}
		s.armClaimHold(1, v)
		if got := s.claimHeldUntil(1); got != 0 {
			t.Fatalf("armClaimHold(%d) left hold=%d, want it cleared", v, got)
		}
	}
}

// A corrupt stored value must fail open: the tick asks the Origin rather than
// silently switching claiming off for that Account.
func TestClaimHeldUntilFailsOpenOnGarbage(t *testing.T) {
	s := newHoldServer(t)
	for _, v := range []string{"garbage", "", " 123 ", "9999999999999999999999"} {
		s.store.SetSetting(claimHoldKey(1), v)
		if got := s.claimHeldUntil(1); got != 0 {
			t.Fatalf("stored %q read back as %d, want 0 (fail open)", v, got)
		}
	}
}

// The key is per Account: one Account's hold must not silence another's.
func TestClaimHoldIsPerAccount(t *testing.T) {
	s := newHoldServer(t)
	s.armClaimHold(1, time.Now().Add(time.Hour).Unix())

	if s.claimHeldUntil(2) != 0 {
		t.Fatal("account 2 inherited account 1's hold")
	}
}
