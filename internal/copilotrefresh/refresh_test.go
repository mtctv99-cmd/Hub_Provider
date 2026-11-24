package copilotrefresh

import (
	"testing"
	"time"

	"sever_provider/internal/identity"
)

// M2 regression: an AUTH_REQUIRED profile must not launch Chrome every tick.
func TestRefreshDueSkipsTerminalAuthRequiredWithBackoff(t *testing.T) {
	resetRefreshBackoffs()
	defer resetRefreshBackoffs()
	now := time.Now()

	due, next, logSkip := refreshDue("copilot022", identity.StatusAuthRequired, false, now)
	if due {
		t.Fatal("terminal AUTH_REQUIRED attempted immediately")
	}
	if !logSkip {
		t.Fatal("first terminal observation must be logged once")
	}
	if got := next.Sub(now); got != refreshBaseBackoff {
		t.Fatalf("next retry in %v, want %v", got, refreshBaseBackoff)
	}

	due, _, logSkip = refreshDue("copilot022", identity.StatusAuthRequired, false, now.Add(time.Minute))
	if due || logSkip {
		t.Fatalf("5-minute tick retried/logged again: due=%v log=%v", due, logSkip)
	}

	at := now.Add(refreshBaseBackoff + time.Second)
	due, _, _ = refreshDue("copilot022", identity.StatusAuthRequired, false, at)
	if !due {
		t.Fatal("backoff window elapsed but refresh still skipped")
	}
	if refreshFailed("copilot022", at) {
		t.Fatal("second failure within a day must not be logged again")
	}
	due, next, logSkip = refreshDue("copilot022", identity.StatusAuthRequired, false, at.Add(time.Minute))
	if due || logSkip {
		t.Fatalf("escalated backoff not applied: due=%v log=%v next=%v", due, logSkip, next)
	}
	if got := next.Sub(at); got != refreshBackoffDelay(2) {
		t.Fatalf("escalated delay=%v, want %v", got, refreshBackoffDelay(2))
	}
}

func TestRefreshBackoffDelayIsMonotonicAndCapped(t *testing.T) {
	if got := refreshBackoffDelay(1); got != refreshBaseBackoff {
		t.Fatalf("delay(1)=%v, want %v", got, refreshBaseBackoff)
	}
	if refreshBackoffDelay(2) <= refreshBackoffDelay(1) {
		t.Fatal("backoff did not grow")
	}
	if got := refreshBackoffDelay(20); got != refreshMaxBackoff {
		t.Fatalf("delay(20)=%v, want cap %v", got, refreshMaxBackoff)
	}
}

func TestRefreshDueHealthAndForce(t *testing.T) {
	resetRefreshBackoffs()
	defer resetRefreshBackoffs()
	now := time.Now()

	if due, _, _ := refreshDue("healthy", identity.StatusHealthy, false, now); !due {
		t.Fatal("healthy credential must be eligible")
	}
	if due, _, _ := refreshDue("terminal", identity.StatusAuthRequired, true, now); !due {
		t.Fatal("manual refresh (lead < 0) must bypass backoff")
	}

	due, _, _ := refreshDue("terminal", identity.StatusAuthRequired, false, now)
	if due {
		t.Fatal("terminal credential should back off")
	}
	clearRefreshBackoff("terminal")
	if due, _, _ := refreshDue("terminal", identity.StatusHealthy, false, now); !due {
		t.Fatal("cleared backoff must be eligible again")
	}
}

func TestRefreshFailedReportsFirstFailureAndDailyAfter(t *testing.T) {
	resetRefreshBackoffs()
	defer resetRefreshBackoffs()
	now := time.Now()
	if !refreshFailed("c1", now) {
		t.Fatal("first failure must be reported")
	}
	if refreshFailed("c1", now.Add(time.Minute)) {
		t.Fatal("repeated failure must be suppressed")
	}
	if !refreshFailed("c1", now.Add(refreshLogInterval+time.Minute)) {
		t.Fatal("failure must be reported again the next day")
	}
}
