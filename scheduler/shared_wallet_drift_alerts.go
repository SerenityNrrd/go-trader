package main

import (
	"fmt"
	"math"
	"os"
	"sync"
	"time"
)

// sharedWalletDriftTolerance is the cent-exact reconciliation tolerance (#918).
// Once per-strategy values are exchange-derived, Σ member value should equal
// the real account balance to the cent every cycle, so any excess is a genuine
// accounting/attribution bug (an on-chain position no member owns, a weight
// that summed to zero), NOT expected mark/fee noise. One cent absorbs benign
// float rounding only.
const sharedWalletDriftTolerance = 0.01

// sharedWalletDriftEntry is one slot in the per-wallet drift tracker.
type sharedWalletDriftEntry struct {
	count          int
	lastNotifiedAt time.Time
	alerted        bool
	lastDriftCents int64 // signature: re-alert when the drift magnitude shifts
}

// SharedWalletDriftTracker throttles the cent-exact drift alarm per shared
// wallet so a persistent attribution bug does not spam the operator every
// cycle. Unlike the signal-script tracker (#829), it alerts on the FIRST
// detection — exchange-derived values should never drift in normal operation,
// so there is no transient-noise window to wait out. All state is in-memory and
// resets on restart.
type SharedWalletDriftTracker struct {
	mu      sync.Mutex
	entries map[string]*sharedWalletDriftEntry
}

// Record registers an over-tolerance drift for walletKey and reports whether
// this cycle should fire an operator alert, along with the post-increment
// consecutive-detection count. The first detection always alerts; subsequent
// detections re-throttle (a materially changed drift, every 10th cycle, or once
// an hour) while the drift persists.
func (t *SharedWalletDriftTracker) Record(walletKey string, drift float64, now time.Time) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]*sharedWalletDriftEntry)
	}
	e := t.entries[walletKey]
	if e == nil {
		e = &sharedWalletDriftEntry{}
		t.entries[walletKey] = e
	}
	driftCents := int64(math.Round(drift * 100))
	// "Materially changed" = the drift moved by more than a cent since the last
	// notification, so a slowly-worsening bug re-surfaces.
	sigChanged := absInt64(driftCents-e.lastDriftCents) > 1
	e.count++
	e.lastDriftCents = driftCents

	shouldNotify := false
	switch {
	case !e.alerted:
		shouldNotify = true // first detection
	case sigChanged:
		shouldNotify = true
	case e.count%10 == 0:
		shouldNotify = true
	case !e.lastNotifiedAt.IsZero() && now.Sub(e.lastNotifiedAt) >= time.Hour:
		shouldNotify = true
	}
	if shouldNotify {
		e.alerted = true
		e.lastNotifiedAt = now
	}
	return shouldNotify, e.count
}

// Clear resets the drift streak for walletKey after a within-tolerance cycle
// and reports whether the wallet had alerted (a recovery notice is warranted)
// plus the streak length that just ended.
func (t *SharedWalletDriftTracker) Clear(walletKey string) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		return false, 0
	}
	e := t.entries[walletKey]
	if e == nil {
		return false, 0
	}
	recovered := e.alerted
	priorCount := e.count
	delete(t.entries, walletKey)
	return recovered, priorCount
}

// sharedWalletDriftTracker is the package-level singleton; resets on restart.
var sharedWalletDriftTracker = &SharedWalletDriftTracker{}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// sharedWalletKeyLabel renders a wallet key as "{platform}/{account}" for
// operator messages. The account address is shown in full (it is a public
// on-chain address / API-key identifier already present in other operator logs).
func sharedWalletKeyLabel(key SharedWalletKey) string {
	return fmt.Sprintf("%s/%s", key.Platform, key.Account)
}

func formatSharedWalletDriftAlert(key SharedWalletKey, balance, memberSum, drift float64, count int) string {
	return fmt.Sprintf(
		"**SHARED-WALLET DRIFT** %s (pid=%d, %d consecutive): Σ member value $%.2f vs real balance $%.2f — diff $%+.2f exceeds $%.2f tolerance. Exchange-derived rows should reconcile exactly; this indicates an attribution/accounting bug (orphan position or weighting).",
		sharedWalletKeyLabel(key), os.Getpid(), count, memberSum, balance, drift, sharedWalletDriftTolerance)
}

func formatSharedWalletDriftRecovered(key SharedWalletKey, priorCount int) string {
	return fmt.Sprintf(
		"**SHARED-WALLET DRIFT RESOLVED** %s (pid=%d): per-strategy values reconcile to the account balance again after %d cycles of drift.",
		sharedWalletKeyLabel(key), os.Getpid(), priorCount)
}

// reportSharedWalletDrift evaluates each reconciled wallet's drift against the
// cent tolerance and fires throttled operator alerts (first detection, then
// backed-off) or a one-shot recovery notice. Drift is always recorded so counts
// and recovery state stay accurate even with no notifier backends. Wallets not
// reconciled this cycle (balance fetch failed) are absent from results and so
// are neither alarmed nor recovery-cleared — their prior streak (if any) is
// preserved, matching the "skip on fetch failure, don't false-alarm" rule.
func reportSharedWalletDrift(notifier *MultiNotifier, results []sharedWalletDriftResult) {
	now := time.Now().UTC()
	for _, r := range results {
		label := sharedWalletKeyLabel(r.Key)
		if math.Abs(r.Drift) > sharedWalletDriftTolerance {
			shouldNotify, count := sharedWalletDriftTracker.Record(label, r.Drift, now)
			fmt.Printf("[WARN] shared-wallet %s drift $%+.2f (Σ members $%.2f vs balance $%.2f)\n",
				label, r.Drift, r.MemberSum, r.Balance)
			if !shouldNotify || notifier == nil || !notifier.HasBackends() {
				continue
			}
			msg := formatSharedWalletDriftAlert(r.Key, r.Balance, r.MemberSum, r.Drift, count)
			notifier.SendToAllChannels(msg)
			notifier.SendOwnerDM(msg)
			continue
		}
		recovered, priorCount := sharedWalletDriftTracker.Clear(label)
		if !recovered || notifier == nil || !notifier.HasBackends() {
			continue
		}
		msg := formatSharedWalletDriftRecovered(r.Key, priorCount)
		notifier.SendToAllChannels(msg)
		notifier.SendOwnerDM(msg)
	}
}
