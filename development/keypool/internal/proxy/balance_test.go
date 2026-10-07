package proxy

import (
	"encoding/json"
	"testing"
	"time"
)

// Live captures from ollama.com 2026-10-07 (key redacted from probing, shapes verbatim).

// legacyBalance is the actual /api/balance response shape for this account's
// legacy plan (session + weekly limits).
const legacyBalance = `{
  "included": {
    "session": {"remaining_percent": 95.14, "resets_at": "2026-10-07T08:00:00Z"},
    "weekly": {"remaining_percent": 82.16, "resets_at": "2026-10-12T00:00:00Z"}
  },
  "purchased": {"balance_usd": 0}
}`

// newPlanBalance is the documented shape for new plans (dollars, no session/weekly).
const newPlanBalance = `{
  "included": {
    "balance_usd": 72.5,
    "allowance_usd": 100,
    "period": {"from": "2026-09-15T09:30:00Z", "until": "2026-10-15T09:30:00Z"}
  },
  "purchased": {"balance_usd": 25}
}`

func TestBalancePayload_LegacyShape(t *testing.T) {
	var b balancePayload
	if err := json.Unmarshal([]byte(legacyBalance), &b); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if b.Included.Session == nil || b.Included.Weekly == nil {
		t.Fatal("legacy shape must populate session and weekly pointers")
	}
	if b.Included.Session.RemainingPercent != 95.14 {
		t.Errorf("session remaining = %v, want 95.14", b.Included.Session.RemainingPercent)
	}
	if b.Included.Weekly.RemainingPercent != 82.16 {
		t.Errorf("weekly remaining = %v, want 82.16", b.Included.Weekly.RemainingPercent)
	}
	sr, err := time.Parse(time.RFC3339, b.Included.Session.ResetsAt)
	if err != nil || sr.UTC().Hour() != 8 {
		t.Errorf("session resets_at parse = %v err %v", sr, err)
	}
	wr, err := time.Parse(time.RFC3339, b.Included.Weekly.ResetsAt)
	if err != nil || wr.UTC().Day() != 12 {
		t.Errorf("weekly resets_at parse = %v err %v", wr, err)
	}
	if b.Purchased.BalanceUSD != 0 {
		t.Errorf("purchased = %v, want 0", b.Purchased.BalanceUSD)
	}
}

func TestBalancePayload_NewPlanShape(t *testing.T) {
	var b balancePayload
	if err := json.Unmarshal([]byte(newPlanBalance), &b); err != nil {
		t.Fatalf("unmarshal new plan: %v", err)
	}
	// dollars present, session/weekly absent: bars must render "—", not 0%
	if b.Included.Session != nil || b.Included.Weekly != nil {
		t.Error("new-plan shape must leave session/weekly pointers nil")
	}
	if b.Included.BalanceUSD != 72.5 || b.Purchased.BalanceUSD != 25 {
		t.Errorf("dollars = %v/%v, want 72.5/25", b.Included.BalanceUSD, b.Purchased.BalanceUSD)
	}
}

func TestAcquireLeastUsed_MostRemainingWins(t *testing.T) {
	p := &keyPool{selection: "least_used", keys: []*keyState{
		{key: "a", quotaKnown: true, weeklyRemaining: 40},
		{key: "b", quotaKnown: true, weeklyRemaining: 90},
		{key: "c", quotaKnown: true, weeklyRemaining: 60},
	}}
	got, idx, err := p.Acquire()
	if err != nil || got != "b" || idx != 1 {
		t.Errorf("Acquire = %q/%d/%v, want b/1 (most weekly remaining)", got, idx, err)
	}
}

func TestAcquireLeastUsed_FallsBackOnUnknown(t *testing.T) {
	p := &keyPool{selection: "least_used", keys: []*keyState{
		{key: "a", quotaKnown: true, weeklyRemaining: 40, useCount: 9},
		{key: "b", useCount: 2}, // quota unknown: fall back to useCount
	}}
	got, _, err := p.Acquire()
	if err != nil || got != "b" {
		t.Errorf("Acquire = %q/%v, want b (useCount fallback while quota unknown)", got, err)
	}
}