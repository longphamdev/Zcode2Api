package gateway

// scheduler.go — background keep-alive jobs:
//   - daily warm: every hour, warm accounts not yet warmed today (the
//     upstream dedups daily-active by device_mid+date, so re-sending is the
//     official client shape);
//   - quota monitor: periodic billing refresh (QUOTA_REFRESH_MIN, 0=off) so
//     the dashboard and dead-marks stay fresh without pressing Refresh.

import (
	"context"
	"log"
	"strconv"
	"strings"
	"time"
)

// startSchedulers launches the background loops (idempotent, guarded).
func (s *Server) startSchedulers() {
	s.warmOnce.Do(func() {
		go s.dailyWarmLoop()
		go s.claimTickLoop()
	})
	if s.quotaRefreshMin > 0 {
		s.quotaOnce.Do(func() {
			go s.quotaMonitorLoop()
		})
	}
	s.precheckOnce.Do(func() {
		go s.chatPrecheckLoop()
	})
}

func (s *Server) dailyWarmLoop() {
	for {
		s.warmDueAccounts()
		time.Sleep(time.Hour)
	}
}

func (s *Server) claimTickLoop() {
	for {
		time.Sleep(claimTick)
		s.claimDueAccounts()
	}
}

// claimTick is how often the claim step is retried for an account that has
// already been warmed today. Campaigns are first-come-first-served and quota
// limited, so a daily gate loses the race outright: the reference client polls
// every five minutes. The warm (activation events) gate stays daily — the
// Origin dedups those by device_mid+date — but claiming is cheap and idempotent
// (an owned plan is skipped), so it can ride a much shorter beat.
const claimTick = 5 * time.Minute

// claimDueAccounts runs only the claim step, for accounts warmed today. The
// warm date gate is deliberately not consulted here.
func (s *Server) claimDueAccounts() {
	s.mu.Lock()
	claim := s.autoClaim
	s.mu.Unlock()
	if !claim {
		return
	}
	accs, err := s.store.Accounts()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, a := range accs {
		if a.Status != "" {
			continue
		}
		// A campaign already won needs no polling until its window closes.
		// Without this the tick keeps asking the Origin for an Account that
		// holds the plan — three requests every five minutes, all day, to be
		// told "already have".
		holdKey := "claim_hold_" + strconv.FormatInt(a.ID, 10)
		if until, _ := strconv.ParseInt(s.store.Setting(holdKey, "0"), 10, 64); until > now {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		claimed, problems, hold := s.up.ClaimOfferedPlansWithHold(ctx, a.JWT, a.DeviceMid)
		cancel()
		if hold > 0 {
			// Capped, deliberately unlike the reference client. Its success
			// hold is the raw ends_at, so a long plan stops its poll for that
			// whole term; here a single far-future ends_at would switch the
			// claim tick off for every future campaign and leave no trace.
			// A day's cap still removes the 288-polls-a-day waste while
			// guaranteeing the tick re-asks soon enough to catch a new one.
			if maxHold := now + int64((24 * time.Hour).Seconds()); hold > maxHold {
				hold = maxHold
			}
			s.store.SetSetting(holdKey, strconv.FormatInt(hold, 10))
		}
		if claimed != "" || (problems != "" && !strings.HasPrefix(problems, "already have:")) {
			log.Printf("[claim-tick] %s claimed=%q problems=%q hold=%d", maskJWT(a.JWT), claimed, problems, hold)
		}
	}
}

// warmDueAccounts warms every account whose last warm date is not today.
func (s *Server) warmDueAccounts() {
	accs, err := s.store.Accounts()
	if err != nil {
		return
	}
	today := time.Now().Format("2006-01-02")
	for _, a := range accs {
		if a.Status != "" { // expired/revoked/hidden — skip
			continue
		}
		// per-account date stored in settings: warm_last_<id>
		if s.store.Setting("warm_last_"+strconv.FormatInt(a.ID, 10), "") == today {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		s.mu.Lock()
		claim := s.autoClaim
		s.mu.Unlock()
		rep := s.up.WarmAccountOpts(ctx, a.JWT, a.DeviceMid, claim)
		cancel()
		s.store.SetSetting("warm_last_"+strconv.FormatInt(a.ID, 10), today)
		log.Printf("[warm-daily] %s %s", maskJWT(a.JWT), rep.Plans)
	}
}

func (s *Server) quotaMonitorLoop() {
	interval := time.Duration(s.quotaRefreshMin) * time.Minute
	for {
		time.Sleep(interval)
		checked, dropped := s.refreshAllBilling()
		if checked > 0 || dropped > 0 {
			log.Printf("[quota-monitor] checked=%d expired=%d", checked, dropped)
		}
	}
}
