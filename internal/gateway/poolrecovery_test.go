package gateway

import (
	"testing"
	"time"

	"zcode2api/internal/store"
)

func TestRecoveryAtReadyWhenAccountServable(t *testing.T) {
	now := time.Now()
	accs := []store.Account{{ID: 1}}

	if until, waiting := recoveryAt(accs, nil, "GLM-5.3", now); waiting {
		t.Errorf("reported a wait of %s, but an account is servable now", until)
	}
}

func TestRecoveryAtReportsEarliestCooldown(t *testing.T) {
	now := time.Now()
	accs := []store.Account{
		{ID: 1, CoolingUntil: now.Add(16 * time.Minute).Unix()},
		{ID: 2, CoolingUntil: now.Add(13 * time.Minute).Unix()},
	}

	until, waiting := recoveryAt(accs, nil, "GLM-5.3", now)
	if !waiting {
		t.Fatal("reported no wait, but every account is cooling")
	}
	if d := until.Sub(now); d < 12*time.Minute || d > 14*time.Minute {
		t.Errorf("recovery in %s, want about 13m (the earlier of the two)", d)
	}
}

func TestRecoveryAtEmptyPoolIsNotAWait(t *testing.T) {
	if _, waiting := recoveryAt(nil, nil, "GLM-5.3", time.Now()); waiting {
		t.Error("an empty pool is nothing to wait for")
	}
}

func TestRecoveryAtAllExpiredIsNotAWait(t *testing.T) {
	now := time.Now()
	// Each carries a future CoolingUntil as well, so the test fails if Status
	// stops being consulted — a bare expired row would pass either way because
	// its zero CoolingUntil already yields no wait.
	accs := []store.Account{
		{ID: 1, Status: "expired", CoolingUntil: now.Add(10 * time.Minute).Unix()},
		{ID: 2, Status: "revoked", CoolingUntil: now.Add(20 * time.Minute).Unix()},
	}

	if until, waiting := recoveryAt(accs, nil, "GLM-5.3", now); waiting {
		t.Errorf("status marks an account as never recovering; got a wait until %s", until)
	}
}

func TestRecoveryAtRespectsDeadMarker(t *testing.T) {
	now := time.Now()
	accs := []store.Account{{ID: 1}}
	dead := map[int64]int64{1: now.Add(20 * time.Minute).Unix()}

	until, waiting := recoveryAt(accs, dead, "GLM-5.3", now)
	if !waiting {
		t.Fatal("a model-dead account should report a wait")
	}
	if d := until.Sub(now); d < 19*time.Minute {
		t.Errorf("recovery in %s, want ~20m from the dead marker", d)
	}
}

// A servable account beats any wait: the pool is usable now even though another
// account is benched.
func TestRecoveryAtServableBeatsCooldown(t *testing.T) {
	now := time.Now()
	accs := []store.Account{
		{ID: 1, CoolingUntil: now.Add(30 * time.Minute).Unix()},
		{ID: 2},
	}

	if _, waiting := recoveryAt(accs, nil, "GLM-5.3", now); waiting {
		t.Error("a servable account means no wait is needed")
	}
}

// An account excluded for this model does not contribute a recovery time for it.
func TestRecoveryAtIgnoresModelExcluded(t *testing.T) {
	now := time.Now()
	accs := []store.Account{
		{ID: 1, CoolingUntil: now.Add(10 * time.Minute).Unix(), DisabledModels: "glm53"},
	}

	if _, waiting := recoveryAt(accs, nil, "GLM-5.3", now); waiting {
		t.Error("an account excluded for this model never recovers for it")
	}
}

// shouldHold decides whether sitting out the rest of the hold window can help.
// A recovery beyond the window means the wait is pure loss.
func TestShouldHold(t *testing.T) {
	now := time.Now()
	holdUntil := now.Add(10 * time.Minute)

	cases := []struct {
		name     string
		recover  time.Time
		waiting  bool
		wantHold bool
	}{
		{"recovery inside the window", now.Add(2 * time.Minute), true, true},
		{"recovery exactly at the edge", holdUntil, true, true},
		{"recovery beyond the window", now.Add(16 * time.Minute), true, false},
		{"nothing to wait for", time.Time{}, false, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldHold(c.recover, c.waiting, holdUntil); got != c.wantHold {
				t.Errorf("shouldHold(%s, waiting=%v) = %v, want %v",
					c.recover.Sub(now).Round(time.Second), c.waiting, got, c.wantHold)
			}
		})
	}
}
