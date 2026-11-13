package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"zcode2api/internal/config"
	"zcode2api/internal/store"
)

// A single decision answers both questions the pool asks: can this Account serve
// this model now, and if not, why — and when, if the reason is temporary.
func TestAccountEligibilityReasons(t *testing.T) {
	now := time.Now()
	nowUnix := now.Unix()
	coolingUntil := now.Add(5 * time.Minute).Unix()

	cases := []struct {
		name   string
		acc    store.Account
		dead   map[string]int64
		want   eligibilityReason
		wantAt int64 // expected "until" for temporary reasons; 0 = none
	}{
		{
			name: "healthy",
			acc:  store.Account{ID: 1},
			want: eligibleNow,
		},
		{
			name: "expired",
			acc:  store.Account{ID: 1, Status: "expired"},
			want: ineligibleForever,
		},
		{
			name: "revoked",
			acc:  store.Account{ID: 1, Status: "revoked"},
			want: ineligibleForever,
		},
		{
			name: "model excluded",
			acc:  store.Account{ID: 1, DisabledModels: "glm53"},
			want: ineligibleForever,
		},
		{
			name: "plan lacks model",
			acc:  store.Account{ID: 1, QuotaJSON: `[{"name":"other","limit":10,"remaining":10}]`},
			want: ineligibleForever,
		},
		{
			name: "quota exhausted",
			acc:  store.Account{ID: 1, QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":0}]`},
			want: ineligibleForever,
		},
		{
			name:   "cooling",
			acc:    store.Account{ID: 1, CoolingUntil: coolingUntil},
			want:   ineligibleUntil,
			wantAt: coolingUntil,
		},
		{
			name:   "model-dead marker",
			acc:    store.Account{ID: 1},
			dead:   map[string]int64{"1:glm53": nowUnix + 600},
			want:   ineligibleUntil,
			wantAt: nowUnix + 600,
		},
		{
			name: "plan has model, quota remains",
			acc:  store.Account{ID: 1, QuotaJSON: `[{"name":"GLM-5.3","limit":10,"remaining":3}]`},
			want: eligibleNow,
		},
		{
			name: "plan never scanned",
			acc:  store.Account{ID: 1},
			want: eligibleNow,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := accountEligibility(c.acc, "GLM-5.3", c.dead, now)
			if got.reason != c.want {
				t.Fatalf("reason = %v, want %v", got.reason, c.want)
			}
			if c.want == ineligibleUntil && got.until.Unix() != c.wantAt {
				t.Errorf("until = %d, want %d", got.until.Unix(), c.wantAt)
			}
		})
	}
}

// A model-dead marker for a different model says nothing about this one.
func TestAccountEligibilityIgnoresOtherModelsMarkers(t *testing.T) {
	now := time.Now()
	dead := map[string]int64{"1:glm53flash": now.Add(10 * time.Minute).Unix()}

	if got := accountEligibility(store.Account{ID: 1}, "GLM-5.3", dead, now); got.reason != eligibleNow {
		t.Errorf("reason = %v, want eligibleNow; another model's marker is not this one's", got.reason)
	}
}

// Refusing an Account for spent quota must also bench it, so the next walk does
// not re-ask the Origin about an Account known to be empty. That side effect
// belongs to the selection path, not to the pure eligibility decision — which is
// exactly why it is easy to drop during a refactor like this one.
func TestPickExceptBenchesExhaustedQuota(t *testing.T) {
	db, err := store.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	acc, err := db.Add("jwt.a", "a")
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := db.UpdateQuota(acc.ID, store.Quota{
		Remaining: 0, Limit: 10,
		Entitlements: []map[string]any{{
			"name": "GLM-5.3", "limit": 10, "remaining": 0, "period_end": time.Now().Add(24 * time.Hour).Unix(),
		}},
	}); err != nil {
		t.Fatalf("set quota: %v", err)
	}

	srv := &Server{
		store:     db,
		catalog:   NewCatalog(nil),
		stickyAcc: map[string]int64{},
		quota1005: map[string]int{},
		chatOK:    map[int64]time.Time{},
		cfg:       config.Config{RequestHold: time.Minute, AccountMaxTry: 1},
	}

	if picked, _ := srv.pickExcept("GLM-5.3", nil); picked != nil {
		t.Fatalf("picked an exhausted Account: %+v", picked)
	}
	if dead := db.DeadSet(); !dead["1:glm53"] {
		t.Errorf("the exhausted Account was not benched; dead set = %v", dead)
	}
}
