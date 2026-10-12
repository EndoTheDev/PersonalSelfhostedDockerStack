// Package proxy is the allocate.d core: key rotation, retry, stats, and an
// HTTP server. It runs inside the allocate_d-server.exe daemon. The Flutter
// desktop app (allocate_d.exe) consumes state over HTTP (/stats, /events SSE)
// — it does NOT share a process with the proxy.
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the allocate.d daemon version. Bump on release. Surfaced via
// /healthz and the settings page info bar.
const Version = "0.1.0"

// ---------- config ----------

// Duration is a time.Duration that JSON-encodes as an integer number of
// seconds (60, 120) instead of nanoseconds. Stored in the settings table as
// part of the config JSON blob. ponytail: ceiling — sub-second cooldowns
// can't be represented in JSON; fine, nobody needs 500ms on a benched key.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(int64(time.Duration(d) / time.Second))
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var secs int64
	if err := json.Unmarshal(b, &secs); err != nil {
		return err
	}
	*d = Duration(secs) * Duration(time.Second)
	return nil
}

// Config holds the daemon configuration. Upstream, Cooldown, and Port are
// store-backed (settings table, key "config") — env vars seed the store on
// first run, then the store is the source of truth. MasterKey is env-only
// (it decrypts the store — chicken/egg) and excluded from JSON.
type Config struct {
	Upstream     string   `json:"upstream"`
	Cooldown     Duration `json:"cooldown"`
	Timeout      Duration `json:"timeout"` // streaming idle timeout; 0 disables
	Port         string   `json:"port"`
	KeySelection string   `json:"key_selection"` // "lru" (default) or "least_used"
	MasterKey    []byte   `json:"-"`
}

// LoadEnv reads a .env file (KEY=VALUE lines) into the process environment.
// Existing env vars win — file only fills gaps. ponytail: minimal parser,
// no quoting/escape handling (keys are simple strings); upgrade to godotenv
// if you ever need quoted values or shell-style expansion.
func LoadEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // missing .env is fine — not an error
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimRight(scan.Text(), "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if os.Getenv(k) == "" {
			os.Setenv(k, strings.TrimSpace(v))
		}
	}
	if err := scan.Err(); err != nil {
		log.Printf("warning: reading .env: %v", err)
	}
}

// FindEnvUp walks up from the executable's directory looking for a .env file.
// Returns ".env" (CWD fallback) if none found up to 6 levels. The fallback
// lets LoadConfigFromEnv produce the real "ALLOCATED_MASTER_KEY is required" error
// rather than a confusing "no .env found".
func FindEnvUp() string {
	exe, err := os.Executable()
	if err != nil {
		return ".env"
	}
	dir := filepath.Dir(exe)
	for i := 0; i < 6; i++ { // ponytail: ceiling — 6 levels covers repo-root layouts
		candidate := filepath.Join(dir, ".env")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ".env"
}

// LoadConfigFromEnv loads .env from the given path, then reads ALLOCATED_* env
// vars. The master key (ALLOCATED_MASTER_KEY, base64 32-byte AES-256) is
// required — the daemon generates one into .env on first run if missing
// (handled by main.go, not here). ALLOCATED_KEYS is read only as a one-shot
// migration seed; the SQLite store is the source of truth.
// ponytail: env vars use ALLOCATED_ (not ROUTE.D_) because dots in env var
// names are risky on Windows. The display name is still "allocate.d".
func LoadConfigFromEnv(envPath string) (Config, error) {
	LoadEnv(envPath)
	c := Config{
		Upstream: envOr("ALLOCATED_UPSTREAM", "https://ollama.com"),
		Cooldown: Duration(envDurOr("ALLOCATED_COOLDOWN", 60*time.Second)),
		Timeout:  Duration(envDurOr("ALLOCATED_TIMEOUT", 60*time.Second)),
		Port:     envOr("ALLOCATED_PORT", "11435"),
	}
	mk, err := DecodeMasterKey(os.Getenv("ALLOCATED_MASTER_KEY"))
	if err != nil {
		return c, fmt.Errorf("ALLOCATED_MASTER_KEY: %w (main.go should generate one on first run)", err)
	}
	c.MasterKey = mk
	return c, nil
}

// ---------- keyPool (generic; knows nothing about Ollama) ----------

type keyState struct {
	id        int64  // DB row ID, stable across restarts
	name      string // display label, unique per provider ("" = grandfathered)
	key       string // plaintext, decrypted from DB at load time
	lastUsed  time.Time
	coolUntil time.Time
	useCount  int64

	// Real Ollama quota remaining (included.session/weekly.remaining_percent
	// from /api/balance), refreshed by the background goroutine. In-memory
	// only — quota state is transient, not persisted.
	weeklyRemaining   float64   // 0..100
	sessionRemaining  float64   // 0..100
	sessionResetsAt   time.Time
	weeklyResetsAt    time.Time
	purchasedUSD      float64
	quotaKnown        bool
}

type keyPool struct {
	mu        sync.Mutex
	keys      []*keyState
	store     *Store // optional; when set, state changes persist to DB
	selection string // "lru" (default) or "least_used"
}

// newKeyPool builds a pool from decrypted KeyRows. The store is used to
// persist use_count + cool_until updates; nil store = in-memory only
// (used by tests).
func newKeyPool(rows []KeyRow, store *Store) *keyPool {
	now := time.Now()
	states := make([]*keyState, len(rows))
	for i, r := range rows {
		var cool time.Time
		if r.CoolUntil != nil && r.CoolUntil.After(now) {
			cool = *r.CoolUntil
		} else {
			cool = now
		}
		lastUsed := now
		if r.LastUsed != nil && r.LastUsed.After(time.Time{}) {
			lastUsed = *r.LastUsed
		}
		states[i] = &keyState{
			id:        r.ID,
			name:      r.Name,
			key:       r.Plaintext,
			lastUsed:  lastUsed,
			coolUntil: cool,
			useCount:  r.UseCount,
		}
	}
	return &keyPool{keys: states, store: store}
}

// Acquire returns the best non-cooling key and its index, or an error if
// every key is cooling. Selection strategy depends on p.selection:
//   - "lru" (default): least-recently-used (earliest lastUsed)
//   - "least_used": most remaining Ollama quota (weekly remaining_percent from
//     /api/balance, refreshed by the background ticker); falls back to
//     lowest useCount while quota data is unknown (startup, fetch failure);
//     ties broken by earliest lastUsed
func (p *keyPool) Acquire() (string, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var best *keyState
	bestIdx := -1
	less := func(a, b *keyState) bool {
		if p.selection == "least_used" {
			if a.quotaKnown && b.quotaKnown {
				if a.weeklyRemaining != b.weeklyRemaining {
					return a.weeklyRemaining > b.weeklyRemaining
				}
				return a.lastUsed.Before(b.lastUsed)
			}
			if a.useCount != b.useCount {
				return a.useCount < b.useCount
			}
			return a.lastUsed.Before(b.lastUsed)
		}
		return a.lastUsed.Before(b.lastUsed)
	}
	for i, ks := range p.keys {
		if ks.coolUntil.After(now) {
			continue
		}
		if best == nil || less(ks, best) {
			best = ks
			bestIdx = i
		}
	}
	if best == nil {
		return "", -1, errors.New("all keys cooling")
	}
	best.lastUsed = now
	best.useCount++
	if p.store != nil {
		_ = p.store.UpdateKeyState(best.id, best.useCount, nil, now)
	}
	return best.key, bestIdx, nil
}

// isRetryable reports whether a status code is key-specific (worth retrying
// on another key + benching this key). 429 = rate limit, 500+ (except 502)
// = server error. 502 = upstream unreachable (not key-specific — another
// key won't help). 4xx (except 429) = client error (not key-specific).
// ponytail: ceiling — Ollama Cloud only documents 200/400/404/429/500/502;
// if 503/504 appear, they're treated like 500 (retry+bench) as the safe
// default. For fine-grained 429 classification (rate-limit vs quota vs
// deactivated), see roadmap v0.4.0+.
func isRetryable(status int) bool {
	return status == 429 || (status >= 500 && status != 502)
}

// Release marks a key cooling on a retryable status (429 or 5xx except 502).
// Any other status is a no-op (lastUsed was already updated in Acquire).
func (p *keyPool) Release(idx int, status int, cooldown time.Duration) {
	if idx < 0 || idx >= len(p.keys) {
		return
	}
	if !isRetryable(status) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cool := time.Now().Add(cooldown)
	p.keys[idx].coolUntil = cool
	if p.store != nil {
		_ = p.store.UpdateKeyState(p.keys[idx].id, p.keys[idx].useCount, &cool, p.keys[idx].lastUsed)
	}
}

// balancePayload extracts the quota fields keypool needs from the
// /api/balance response. Ollama moved quota limits here from /api/usage
// (2026-10). Legacy plans return included.session/weekly with
// remaining_percent + resets_at; new plans return balance_usd/allowance_usd
// instead — both parse, missing fields stay zero.
// ponytail: if the account ever switches to a new plan, session/weekly bars
// show "—" and only purchased_usd renders; upgrade path = a dollars-based
// least_used comparator.
type balancePayload struct {
	Included struct {
		BalanceUSD  float64 `json:"balance_usd"`
		AllowanceUSD float64 `json:"allowance_usd"`
		Session     *struct {
			RemainingPercent float64  `json:"remaining_percent"`
			ResetsAt         string   `json:"resets_at"`
		} `json:"session"`
		Weekly      *struct {
			RemainingPercent float64  `json:"remaining_percent"`
			ResetsAt         string   `json:"resets_at"`
		} `json:"weekly"`
	} `json:"included"`
	Purchased struct {
		BalanceUSD float64 `json:"balance_usd"`
	} `json:"purchased"`
}

// refreshUsage fetches each key's real Ollama quota remaining
// (included.*.remaining_percent from {upstream}/api/balance) and stores it
// on the key state. Called by the background ticker in NewServer — never on
// the request path. A failed fetch clears quotaKnown so Acquire falls back
// to useCount comparison instead of trusting stale quota data.
// Rate math: N keys x 1 req/min vs balance's documented 10 req/min per user
// cap — fine at any realistic key count. ponytail: don't poll per-request.
func (p *keyPool) refreshUsage(upstream string) {
	p.mu.Lock()
	keys := make([]string, len(p.keys))
	for i, ks := range p.keys {
		keys[i] = ks.key
	}
	p.mu.Unlock()
	for i, raw := range keys {
		known := false
		var weekly, session, purchased float64
		var sessionResets, weeklyResets time.Time
		if body, err := fetchBalance(upstream, raw, 10*time.Second); err == nil {
			var b balancePayload
			if json.Unmarshal(body, &b) == nil {
				if b.Included.Session != nil {
					session = b.Included.Session.RemainingPercent
					sessionResets, _ = time.Parse(time.RFC3339, b.Included.Session.ResetsAt)
				}
				if b.Included.Weekly != nil {
					weekly = b.Included.Weekly.RemainingPercent
					weeklyResets, _ = time.Parse(time.RFC3339, b.Included.Weekly.ResetsAt)
				}
				purchased = b.Purchased.BalanceUSD
				known = true
			}
		}
		p.mu.Lock()
		p.keys[i].weeklyRemaining = weekly
		p.keys[i].sessionRemaining = session
		p.keys[i].sessionResetsAt = sessionResets
		p.keys[i].weeklyResetsAt = weeklyResets
		p.keys[i].purchasedUSD = purchased
		p.keys[i].quotaKnown = known
		p.mu.Unlock()
	}
}

// KeyInfo is a read-only snapshot of one key's state for the dashboard.
// The ID field is the DB row ID (stable across restarts); Index is the
// pool slice position (changes when keys are added/removed). The dashboard
// shows Index; admin endpoints use ID.
type KeyInfo struct {
	ID        int64      `json:"id"`
	Index     int        `json:"index"`
	Name      string     `json:"name"` // display label, unique per provider ("" = unnamed)
	Cooling   bool       `json:"cooling"`
	CoolUntil time.Time  `json:"cool_until"`
	UseCount  int64      `json:"use_count"`
	LastUsed  *time.Time `json:"last_used"` // nil = never used (pre-migration keys)

	// Quota remaining from Ollama /api/balance (0..100). SessionRemaining is
	// display-only; balancing uses WeeklyRemaining. Zero + QuotaKnown=false
	// means the background poll hasn't answered yet.
	SessionRemaining float64   `json:"session_remaining"`
	WeeklyRemaining  float64   `json:"weekly_remaining"`
	SessionResetsAt  *time.Time `json:"session_resets_at,omitempty"`
	WeeklyResetsAt   *time.Time `json:"weekly_resets_at,omitempty"`
	PurchasedUSD     float64   `json:"purchased_usd"`
	QuotaKnown       bool      `json:"quota_known"`
	// Back-compat: used-fraction (0..1) derived from remaining. UI is the
	// only consumer; new fields above are the real data.
	SessionUsage float64 `json:"session_usage"`
	WeeklyUsage  float64 `json:"weekly_usage"`
}

// Snapshot returns a read-only copy of all key states. Lock-held, returns
// a fresh slice — safe to marshal to JSON or hand to a template.
func (p *keyPool) Snapshot() []KeyInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]KeyInfo, len(p.keys))
	for i, ks := range p.keys {
		cooling := ks.coolUntil.After(now)
		var lastUsed *time.Time
		if !ks.lastUsed.IsZero() {
			lu := ks.lastUsed
			lastUsed = &lu
		}
		var sessionResets, weeklyResets *time.Time
		if !ks.sessionResetsAt.IsZero() {
			sr := ks.sessionResetsAt
			sessionResets = &sr
		}
		if !ks.weeklyResetsAt.IsZero() {
			wr := ks.weeklyResetsAt
			weeklyResets = &wr
		}
		out[i] = KeyInfo{
			ID:              ks.id,
			Index:           i,
			Name:            ks.name,
			Cooling:         cooling,
			CoolUntil:       ks.coolUntil,
			UseCount:        ks.useCount,
			LastUsed:        lastUsed,
			SessionRemaining: ks.sessionRemaining,
			WeeklyRemaining:  ks.weeklyRemaining,
			SessionResetsAt:  sessionResets,
			WeeklyResetsAt:   weeklyResets,
			PurchasedUSD:     ks.purchasedUSD,
			QuotaKnown:       ks.quotaKnown,
			// back-compat used-fraction
			SessionUsage:    (100 - ks.sessionRemaining) / 100,
			WeeklyUsage:     (100 - ks.weeklyRemaining) / 100,
		}
	}
	return out
}

// ---------- stats (atomic counters + bounded request log) ----------

type stats struct {
	total      atomic.Int64
	ok         atomic.Int64 // 2xx
	rateLim    atomic.Int64 // 429
	server     atomic.Int64 // 5xx
	retries    atomic.Int64
	compressed atomic.Int64 // requests that were compressed
	saved      atomic.Int64 // cumulative tokens saved
	log        *ringBuffer

	// Upstream network errors (exec failures — resets, stale-conn timeouts).
	// Distinct from server5xx: those got an HTTP response; these never did.
	upstreamErr atomic.Int64

	mu                sync.Mutex // guards lastUpstreamErr + lastUpstreamAt
	lastUpstreamErr   string
	lastUpstreamAt    time.Time
}

func newStats(logSize int) *stats {
	return &stats{log: newRingBuffer(logSize)}
}

// recordUpstreamError bumps the upstream-error counter and stores the short
// error text for the snapshot. err may be nil (no-op).
func (s *stats) recordUpstreamError(err error) {
	if err == nil {
		return
	}
	s.upstreamErr.Add(1)
	s.mu.Lock()
	s.lastUpstreamErr = errShort(err)
	s.lastUpstreamAt = time.Now()
	s.mu.Unlock()
}

// upstreamErrInfo returns the last upstream error text and its time.
func (s *stats) upstreamErrInfo() (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUpstreamErr, s.lastUpstreamAt
}

// errShort reduces an upstream error to a short, stable classification used
// in the dashboard (no full error chains — keep the UI line short).
func errShort(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, ':'); i > 0 && i < 40 {
		msg = msg[:i]
	}
	if len(msg) > 60 {
		msg = msg[:60]
	}
	return msg
}

func (s *stats) record(status int, retried bool) {
	s.total.Add(1)
	if status >= 200 && status < 300 {
		s.ok.Add(1)
	}
	if status == 429 {
		s.rateLim.Add(1)
	}
	if status >= 500 {
		s.server.Add(1)
	}
	if retried {
		s.retries.Add(1)
	}
}

// reclassify adjusts atomic counters when a streaming response is
// reclassified after the fact (e.g. 200 → 502 on stream idle timeout).
// Called from metricReader.onDone — after the event was already recorded.
func (s *stats) reclassify(from, to int) {
	if from >= 200 && from < 300 {
		s.ok.Add(-1)
	}
	if from == 429 {
		s.rateLim.Add(-1)
	}
	if from >= 500 {
		s.server.Add(-1)
	}
	if to >= 200 && to < 300 {
		s.ok.Add(1)
	}
	if to == 429 {
		s.rateLim.Add(1)
	}
	if to >= 500 {
		s.server.Add(1)
	}
}

// ReqEntry is one row in the recent-requests log.
type ReqEntry struct {
	Time               time.Time `json:"time"`
	Key                int       `json:"key"`
	Method             string    `json:"method"`
	Path               string    `json:"path"`
	Status             int       `json:"status"`
	Latency            int64     `json:"latency_ms"`
	Retried            bool      `json:"retried"`
	ErrorMsg           string    `json:"error_msg,omitempty"`
	Model              string    `json:"model,omitempty"`
	PromptTokens       int       `json:"prompt_tokens,omitempty"`
	OutputTokens       int       `json:"output_tokens,omitempty"`
	TotalDuration      int64     `json:"total_duration,omitempty"` // nanoseconds
	LoadDuration       int64     `json:"load_duration,omitempty"`
	PromptEvalDuration int64     `json:"prompt_eval_duration,omitempty"`
	EvalDuration       int64     `json:"eval_duration,omitempty"`
	Compressed         bool      `json:"compressed,omitempty"`
	TokensSaved        int64     `json:"tokens_saved,omitempty"`
}

// ringBuffer is a fixed-capacity circular buffer for ReqEntry. Bounded
// memory — old entries fall off the back. ponytail: single mutex, no
// grow/shrink; if you need >10k entries, a sharded ring would reduce lock
// contention, but 100 is uncontended.
type ringBuffer struct {
	mu    sync.Mutex
	buf   []ReqEntry
	head  int // index of next write
	count int // number of valid entries (<= len(buf))
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{buf: make([]ReqEntry, size)}
}

func (r *ringBuffer) Add(e ReqEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.head] = e
	r.head = (r.head + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
}

// Snapshot returns entries newest-first. Allocates a fresh slice — safe to
// hand to a template or marshal.
func (r *ringBuffer) Snapshot() []ReqEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ReqEntry, r.count)
	for i := 0; i < r.count; i++ {
		idx := (r.head - 1 - i + len(r.buf)) % len(r.buf)
		out[i] = r.buf[idx]
	}
	return out
}

// UpdateMetrics backfills token metrics into the most recent entry matching
// the given time. Called by metricReader.onDone when a streaming response
// completes. ponytail: ceiling — O(n) scan of 100 entries, called once per
// streaming request. Fine at allocate.d's load; if RPS makes this hot, add
// an index or use the entry's EventID instead.
// If reclassify is true, the entry's status and error message are updated
// (e.g. 200 → 502 on stream idle timeout).
func (r *ringBuffer) UpdateMetrics(t time.Time, m tokenMetrics, reclassify bool, newStatus int, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < r.count; i++ {
		idx := (r.head - 1 - i + len(r.buf)) % len(r.buf)
		if r.buf[idx].Time.Equal(t) {
			if reclassify {
				r.buf[idx].Status = newStatus
				r.buf[idx].ErrorMsg = errMsg
				return
			}
			r.buf[idx].Model = m.Model
			r.buf[idx].PromptTokens = m.PromptTokens
			r.buf[idx].OutputTokens = m.OutputTokens
			r.buf[idx].TotalDuration = m.TotalDuration
			r.buf[idx].LoadDuration = m.LoadDuration
			r.buf[idx].PromptEvalDuration = m.PromptEvalDuration
			r.buf[idx].EvalDuration = m.EvalDuration
			if m.TokensSaved > 0 {
				r.buf[idx].TokensSaved = m.TokensSaved
			}
			return
		}
	}
}

// ---------- snapshot + notifier + broadcaster ----------

// HealthInfo is the enriched /healthz response. Extends the liveness probe
// with version, runtime, uptime, and port — consumed by the settings page
// info bar. One round-trip, no extra endpoint.
type HealthInfo struct {
	Service string `json:"service"`
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	Go      string `json:"go"`
	Uptime  string `json:"uptime"`
	Port    string `json:"port"`
}

// HealthInfo returns the enriched health payload for /healthz.
func (s *Server) HealthInfo() HealthInfo {
	cfg := s.configSnapshot()
	return HealthInfo{
		Service: "allocate.d",
		OK:      true,
		Version: Version,
		Go:      runtime.Version(),
		Uptime:  time.Since(s.startTime).Round(time.Second).String(),
		Port:    cfg.Port,
	}
}

// ProbeResult is the JSON response for /admin/probe and /admin/keys/{id}/test.
type ProbeResult struct {
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// ProbeUpstream sends a GET to {upstream}/api/tags with a 5s timeout and no
// auth header. Tests upstream reachability, not key validity. Used by
// /admin/probe.
func (s *Server) ProbeUpstream() ProbeResult {
	s.cfgMu.RLock()
	u := s.upstreamURL
	s.cfgMu.RUnlock()
	if u == nil {
		return ProbeResult{Error: "no upstream configured"}
	}
	return probeUpstream(u.String(), "", 5*time.Second)
}

// probeUpstream sends GET {upstream}/api/tags with an optional bearer token and
// timeout. Shared by ProbeUpstream (no auth, 5s) and TestKey (with auth, 10s).
// ponytail: ceiling — /api/tags is the lightest Ollama endpoint (GET, lists
// models). If the upstream isn't Ollama, this may 404 but still confirms
// reachability. Upgrade: configurable probe path per provider.
func probeUpstream(upstream, auth string, timeout time.Duration) ProbeResult {
	start := time.Now()
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, upstream+"/api/tags", nil)
	if err != nil {
		return ProbeResult{Error: "build request: " + err.Error()}
	}
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{Error: "unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	return ProbeResult{
		OK:        resp.StatusCode >= 200 && resp.StatusCode < 400,
		Status:    resp.StatusCode,
		LatencyMs: time.Since(start).Milliseconds(),
	}
}

// KeyUsageResult is one entry in the /admin/usage response.
// Usage is the raw JSON from Ollama's /api/usage (passthrough — not parsed).
type KeyUsageResult struct {
	KeyID   int64           `json:"key_id"`
	KeyName string          `json:"key_name"`
	OK      bool            `json:"ok"`
	Usage   json.RawMessage `json:"usage,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// fetchUsage calls {upstream}/api/usage with raw auth (no Bearer prefix)
// and returns the response body. One call per key — each key is a separate
// Ollama account with its own usage/quota/limits.
func fetchUsage(upstream, key string, timeout time.Duration) (json.RawMessage, error) {
	return fetchUpstream(upstream, "/api/usage", key, timeout)
}

// fetchBalance calls {upstream}/api/balance with raw auth (both raw and
// Bearer verified accepted 2026-10-07) — quota limits moved here from
// /api/usage. fetchUsage stays for the /admin/usage debugging view.
func fetchBalance(upstream, key string, timeout time.Duration) (json.RawMessage, error) {
	return fetchUpstream(upstream, "/api/balance", key, timeout)
}

func fetchUpstream(upstream, path, key string, timeout time.Duration) (json.RawMessage, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, upstream+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API %s returned %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

// TestKey loads a key by ID, sends GET {upstream}/api/tags with the key's
// bearer token and a 10s timeout. Confirms the key is valid against the
// upstream. Used by /admin/keys/{id}/test.
func (s *Server) TestKey(id int64) ProbeResult {
	key, err := s.store.GetKey(id)
	if err != nil {
		return ProbeResult{Error: "key not found"}
	}
	s.cfgMu.RLock()
	u := s.upstreamURL
	s.cfgMu.RUnlock()
	if u == nil {
		return ProbeResult{Error: "no upstream configured"}
	}
	return probeUpstream(u.String(), key.Plaintext, 10*time.Second)
}

// StatsSnapshot is the aggregate counters block of a Snapshot.
type StatsSnapshot struct {
	Total          int64 `json:"total"`
	OK             int64 `json:"ok"`
	Rate429        int64 `json:"rate_429"`
	Server5xx      int64 `json:"server_5xx"`
	Retries        int64 `json:"retries"`
	CompressedReqs int64 `json:"compressed_reqs"`
	TokensSaved    int64 `json:"tokens_saved"`
	UpstreamErrors int64 `json:"upstream_errors"`
	// Last upstream network error (empty string if none yet) + unix seconds.
	LastUpstreamError   string  `json:"last_upstream_error"`
	LastUpstreamErrorAt float64 `json:"last_upstream_error_at"` // 0 = never
}

// InFlightEntry is one currently-running request. The dashboard shows these
// at the top of the recent log with a "running" badge and a live latency
// ticker. ID is monotonic (for stable Vue v-for keys); StartedAt is unix
// seconds with fractional precision for client-side elapsed calculation.
type InFlightEntry struct {
	ID        int64   `json:"id"`
	Method    string  `json:"method"`
	Path      string  `json:"path"`
	KeyIndex  int     `json:"key"`
	Model     string  `json:"model"`
	StartedAt float64 `json:"started_at"`
}

// Snapshot is the full dashboard state. The Flutter app receives this via
// the /stats JSON endpoint and /events SSE stream.
type Snapshot struct {
	Keys     []KeyInfo       `json:"keys"`
	Stats    StatsSnapshot   `json:"stats"`
	Recent   []ReqEntry      `json:"recent"`
	InFlight []InFlightEntry `json:"in_flight"`
}

// broadcaster fans out snapshot events to all subscribed SSE clients on /events.
// ponytail: single mutex, buffered chans; ceiling = one goroutine per client
// holding the lock during send (mitigated by non-blocking send + 16-msg
// buffer — slow client drops, doesn't block the proxy). Upgrade to
// per-client goroutine if you need backpressure-aware delivery.
type broadcaster struct {
	mu   sync.RWMutex
	subs map[chan []byte]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[chan []byte]struct{})}
}

// Subscribe returns a buffered channel and registers it. The caller passes
// the initial snapshot bytes (already marshaled) so the client sees current
// state without waiting for the next event.
func (b *broadcaster) Subscribe(initial []byte) chan []byte {
	ch := make(chan []byte, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	if initial != nil {
		ch <- initial
	}
	return ch
}

func (b *broadcaster) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// Broadcast sends the snapshot JSON to all subscribers. Non-blocking: slow
// clients drop. Called from Server.notify() on every state change.
func (b *broadcaster) Broadcast(snap []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- snap:
		default: // buffer full — drop
		}
	}
}

// ---------- retryTransport ----------

type retryTransport struct {
	inner http.RoundTripper
	logf  func(format string, args ...any)
	st    *stats
	srv   *Server // for Snapshot() + notifier + live pool/cooldown access
}

func newRetryTransport(logf func(string, ...any), st *stats, srv *Server) *retryTransport {
	return &retryTransport{
		inner: &http.Transport{
			ResponseHeaderTimeout: srv.timeout(), // kills connection stalls (default 60s)
			IdleConnTimeout:      60 * time.Second,
			ForceAttemptHTTP2:     true,
			// Ollama's edge reaps idle/slow HTTP/2 connections. The health-check
			// ping detects a dead connection within SendPingTimeout and closes
			// it (PingTimeout) so a fresh one is dialed before a request lands
			// on a stale connection.
			HTTP2: &http.HTTP2Config{
				SendPingTimeout: 30 * time.Second,
				PingTimeout:     15 * time.Second,
			},
		},
		logf: logf,
		st:   st,
		srv:  srv,
	}
}

// pool returns the current keyPool, read live under the server's poolMu so
// admin-triggered ReloadPool calls are picked up without restarting.
func (t *retryTransport) pool() *keyPool {
	t.srv.poolMu.RLock()
	defer t.srv.poolMu.RUnlock()
	return t.srv.pool
}

// record writes one request outcome to stats, the in-memory ring buffer,
// and (if the store is configured) the events table. keyIdx is the pool
// slice index, or -1 if no key was acquired. errMsg is the upstream error
// message extracted from the response body ("" for success / no body).
// m holds token metrics (populated for non-streaming, empty for streaming —
// streaming metrics are backfilled by metricReader.onDone via
// UpdateEventMetrics). Returns the DB event row ID (0 if no store) and the
// entry creation time (for ring buffer matching by metricReader).
func (t *retryTransport) record(keyIdx, status int, method, path string, latency time.Duration, retried bool, errMsg string, m tokenMetrics) (int64, time.Time) {
	t.st.record(status, retried)
	pool := t.pool()
	var keyID *int64
	if keyIdx >= 0 && keyIdx < len(pool.keys) {
		id := pool.keys[keyIdx].id
		keyID = &id
	}
	now := time.Now()
	// Compute tokens_saved for non-streaming responses where prompt tokens
	// are already known. For streaming, prompt tokens arrive later and
	// tokens_saved is backfilled by updateEntryMetrics.
	tokensSaved := int64(0)
	compressed := m.PreBytes > 0 && m.PostBytes > 0 && m.PostBytes < m.PreBytes
	if compressed && m.PromptTokens > 0 {
		// ponytail: ceiling — pre_bytes/4 is a rough char-count token estimate
		// (CHARS_PER_TOKEN=4, same constant OmniRoute uses). The post side is
		// real prompt_tokens from the upstream response. Upgrade path: real
		// tokenizer on the pre-compression body (Ollama PR #12030, unmerged).
		estOriginal := int64(m.PreBytes) / 4
		if estOriginal > int64(m.PromptTokens) {
			tokensSaved = estOriginal - int64(m.PromptTokens)
		}
	}
	if compressed {
		t.st.compressed.Add(1)
		t.st.saved.Add(tokensSaved)
	}
	entry := ReqEntry{
		Time:               now,
		Key:                keyIdx,
		Method:             method,
		Path:               path,
		Status:             status,
		Latency:            ms(latency),
		Retried:            retried,
		ErrorMsg:           errMsg,
		Model:              m.Model,
		PromptTokens:       m.PromptTokens,
		OutputTokens:       m.OutputTokens,
		TotalDuration:      m.TotalDuration,
		LoadDuration:       m.LoadDuration,
		PromptEvalDuration: m.PromptEvalDuration,
		EvalDuration:       m.EvalDuration,
		Compressed:         compressed,
		TokensSaved:        tokensSaved,
	}
	t.st.log.Add(entry)
	if t.srv.store != nil {
		eventID, _ := t.srv.store.InsertEvent(Event{
			TS:                 now.UnixMilli(),
			KeyID:              keyID,
			Method:             method,
			Path:               path,
			Status:             status,
			LatencyMs:          ms(latency),
			Retry:              boolToInt(retried),
			ErrorMsg:           errMsg,
			Model:              m.Model,
			PromptTokens:       m.PromptTokens,
			OutputTokens:       m.OutputTokens,
			TotalDuration:      m.TotalDuration,
			LoadDuration:       m.LoadDuration,
			PromptEvalDuration: m.PromptEvalDuration,
			EvalDuration:       m.EvalDuration,
			Compressed:         compressed,
			PreBytes:           m.PreBytes,
			PostBytes:          m.PostBytes,
			TokensSaved:        tokensSaved,
		})
		return eventID, now
	}
	return 0, now
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RoundTrip executes the request with a pool key. On a key-specific error
// (429 or 5xx except 502) it retries once on another key. The body is
// buffered (ponytail: ceiling = one body per in-flight request; fine for
// chat JSON which is KB to low MB. If someone streams 1GB uploads through
// this, buffer first then upgrade to streaming-retry with a temp file or
// io.Pipe.). Upstream error messages are extracted from the response body
// and recorded in the event log. In-flight tracking pushes a notify() at
// request start (so the dashboard shows a "running" row immediately) and
// removes the entry at every exit path. Token metrics are captured for
// non-streaming 2xx responses (readTokenMetrics) and streaming 2xx responses
// (metricReader passthrough — metrics backfilled via updateEntryMetrics
// when the stream completes).
func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	method := req.Method
	path := req.URL.Path

	var bodyBuf []byte
	if req.Body != nil {
		var err error
		bodyBuf, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		req.Body.Close()
	}

	var model string
	if len(bodyBuf) > 0 {
		var reqBody struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(bodyBuf, &reqBody) == nil {
			model = reqBody.Model
		}
	}
	baseMetrics := tokenMetrics{Model: model}

	// Lite compression: 5 structural passes on the request body before
	// forwarding upstream. Compresses once — the retry path reuses the
	// already-compressed bodyBuf.
	if cfg := t.srv.compressionConfig(); cfg.Enabled {
		pre := len(bodyBuf)
		bodyBuf = compressBody(bodyBuf, model, cfg)
		post := len(bodyBuf)
		if post < pre {
			baseMetrics.PreBytes = pre
			baseMetrics.PostBytes = post
		}
	}

	key, keyIdx, err := t.pool().Acquire()
	if err != nil {
		t.record(-1, http.StatusServiceUnavailable, method, path, time.Since(start), false, "all keys cooling", baseMetrics)
		t.srv.notify()
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"error":"all keys cooling"}`)),
		}, nil
	}

	// Cancellable context for the upstream connection. Passed to exec
	// so that a streaming idle timeout can cancel the upstream connection
	// and close the stalled response body. The cancel func is owned by
	// metricReader (Close + timer) — no defer here, it would fire before
	// the reverse proxy reads the streaming body.
	ctx, cancel := context.WithCancel(req.Context())

	// for the dashboard — notify() shows a "running" row immediately.
	inFlightID := t.srv.addInFlight(method, path, keyIdx, model)
	t.srv.notify()

	resp, err := t.exec(ctx, req, key, bodyBuf)
	if err != nil {
		// Network error before headers: a stale/reaped pooled connection is a
		// likely cause (ollama's edge resets idle HTTP/2 conns). The body is
		// already buffered, so one retry on a fresh connection + key is safe.
		// Keys are NOT cooled down for network errors — the key is fine;
		// the pooled connection was stale. Release only handles retryable
		// HTTP statuses, so no unwind is needed here.
		t.st.recordUpstreamError(err) // visible even when the retry recovers
		if k2, i2, err2 := t.pool().Acquire(); err2 == nil {
			resp2, err3 := t.exec(ctx, req, k2, bodyBuf)
			if err3 == nil {
				retryMsg := readErrorMessage(resp2)
				t.pool().Release(i2, resp2.StatusCode, t.srv.cooldown())
				return t.finalize(inFlightID, i2, resp2, cancel, method, path, start, model, true, retryMsg, baseMetrics)
			}
		}
		// Upstream unreachable — return error to ReverseProxy, its
		// ErrorHandler returns a clean 502 to the client.
		cancel()
		t.st.recordUpstreamError(err)
		msg := "upstream unreachable"
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			msg = "upstream timeout"
		}
		return t.fail(inFlightID, keyIdx, http.StatusBadGateway, method, path, start, false, msg, baseMetrics, err)
	}
	if isRetryable(resp.StatusCode) {
		errMsg := readErrorMessage(resp) // extract before draining
		t.pool().Release(keyIdx, resp.StatusCode, t.srv.cooldown())
		if k2, i2, err2 := t.pool().Acquire(); err2 == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			resp2, err2 := t.exec(ctx, req, k2, bodyBuf)
			if err2 != nil {
				cancel()
				msg := "upstream unreachable"
				var netErr net.Error
				if errors.As(err2, &netErr) && netErr.Timeout() {
					msg = "upstream timeout"
				}
				return t.fail(inFlightID, i2, http.StatusBadGateway, method, path, start, true, msg, baseMetrics, err2)
			}
			retryMsg := readErrorMessage(resp2)
			t.pool().Release(i2, resp2.StatusCode, t.srv.cooldown())
			return t.finalize(inFlightID, i2, resp2, cancel, method, path, start, model, true, retryMsg, baseMetrics)
		}
		// No second key available; return the original error response.
		cancel()
		t.srv.removeInFlight(inFlightID)
		t.record(keyIdx, resp.StatusCode, method, path, time.Since(start), false, errMsg, baseMetrics)
		t.srv.notify()
		t.logf("key=%d status=%d retry=false(all_cooling)", keyIdx, resp.StatusCode)
		return resp, nil
	}
	return t.finalize(inFlightID, keyIdx, resp, cancel, method, path, start, model, false, readErrorMessage(resp), baseMetrics)
}

// finalize records a completed request (success or non-retryable error),
// wraps streaming if needed, notifies the dashboard, and returns the response.
// Shared by the retry-success and normal-success paths in RoundTrip.
// cancel is passed so the metricReader can cancel the upstream
// connection on streaming idle timeout.
func (t *retryTransport) finalize(inFlightID int64, keyIdx int, resp *http.Response, cancel context.CancelFunc, method, path string, start time.Time, model string, retried bool, msg string, baseMetrics tokenMetrics) (*http.Response, error) {
	t.srv.removeInFlight(inFlightID)

	// Capture token metrics from non-streaming 2xx JSON responses.
	metrics := tokenMetrics{Model: model, PreBytes: baseMetrics.PreBytes, PostBytes: baseMetrics.PostBytes}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/json") {
			metrics = readTokenMetrics(resp, model)
			metrics.PreBytes = baseMetrics.PreBytes
			metrics.PostBytes = baseMetrics.PostBytes
		}
	}

	eventID, entryTime := t.record(keyIdx, resp.StatusCode, method, path, time.Since(start), retried, msg, metrics)

	// Wrap streaming 2xx responses (NDJSON or SSE) so token metrics from the
	// final chunk are backfilled when the stream completes.
	wrapped := false
	if metrics.OutputTokens == 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/x-ndjson") || strings.HasPrefix(ct, "text/event-stream") {
			sm := &tokenMetrics{Model: model, PreBytes: metrics.PreBytes, PostBytes: metrics.PostBytes}
			idleTimeout := t.srv.timeout()
			mr := &metricReader{
				src:         resp.Body,
				metrics:     sm,
				cancel:      cancel,
				idleTimeout: idleTimeout,
			}
			mr.onDone = func() {
				if mr.timedOut {
					// Stream idle timeout — reclassify as 502.
					t.pool().Release(keyIdx, http.StatusBadGateway, t.srv.cooldown())
					t.st.reclassify(resp.StatusCode, http.StatusBadGateway)
					t.st.log.UpdateMetrics(entryTime, tokenMetrics{}, true, http.StatusBadGateway, "stream idle timeout")
					if t.srv.store != nil && eventID > 0 {
						_ = t.srv.store.UpdateEventStatus(eventID, http.StatusBadGateway, "stream idle timeout")
					}
					t.srv.notify()
					return
				}
				t.srv.updateEntryMetrics(eventID, entryTime, *sm)
			}
			if idleTimeout > 0 {
				mr.timer = time.AfterFunc(idleTimeout, func() {
					mr.timedOut = true
					cancel()
				})
			}
			resp.Body = mr
			wrapped = true
		}
	}

	// Non-streaming response — metricReader won't call cancel.
	// Clean up the context now; the reverse proxy reads the body
	// via the parent request context.
	if !wrapped {
		cancel()
	}

	t.srv.notify()
	t.logf("key=%d status=%d retry=%v", keyIdx, resp.StatusCode, retried)
	return resp, nil
}

// fail records an upstream-unreachable failure (502), notifies, and returns
// the error. Used by RoundTrip's exec-error paths.
func (t *retryTransport) fail(inFlightID int64, keyIdx, status int, method, path string, start time.Time, retried bool, msg string, baseMetrics tokenMetrics, retErr error) (*http.Response, error) {
	t.srv.removeInFlight(inFlightID)
	t.record(keyIdx, status, method, path, time.Since(start), retried, msg, baseMetrics)
	t.srv.notify()
	return nil, retErr
}

func ms(d time.Duration) int64 { return d.Milliseconds() }

// exec clones the request (so retries can re-send), sets the pool key, and
// executes via the inner transport.
func (t *retryTransport) exec(ctx context.Context, req *http.Request, key string, body []byte) (*http.Response, error) {
	c := req.Clone(ctx)
	c.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		c.Body = io.NopCloser(bytes.NewReader(body))
		c.ContentLength = int64(len(body))
	}
	return t.inner.RoundTrip(c)
}

// drainBody reads resp.Body fully, closes it, and re-wraps it so the caller
// can still read the bytes. Returns the body bytes, or nil on read error
// (body replaced with an empty reader). Shared by readErrorMessage and
// readTokenMetrics.
func drainBody(resp *http.Response) []byte {
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

// readErrorMessage reads the response body, extracts the "error" field from
// the JSON, and re-wraps the body so the caller can still read it. Returns
// "" if the body is nil, the status is < 300 (success), or the JSON has no
// error field. Safe for small JSON error bodies (Ollama's format); streaming
// 2xx responses are skipped entirely by the status check.
func readErrorMessage(resp *http.Response) string {
	if resp == nil || resp.Body == nil || resp.StatusCode < 300 {
		return ""
	}
	body := drainBody(resp)
	if body == nil {
		return ""
	}
	var v struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil {
		return v.Error
	}
	return ""
}

// tokenMetrics holds the token + duration metrics extracted from an Ollama
// or OpenAI-compatible response. All duration fields are in nanoseconds
// (raw from Ollama). For OpenAI responses, durations are zero (not provided).
type tokenMetrics struct {
	Model              string
	PromptTokens       int
	OutputTokens       int
	TotalDuration      int64 // nanoseconds
	LoadDuration       int64
	PromptEvalDuration int64
	EvalDuration       int64
	PreBytes           int   // body bytes before compression (0 if not compressed)
	PostBytes          int   // body bytes after compression (0 if not compressed)
	TokensSaved        int64 // estimated tokens saved (backfilled for streaming)
}

// ollamaMetrics is the Ollama native response shape for token + duration
// extraction. Shared by readTokenMetrics (non-streaming) and processLine
// (streaming). Done is only checked by processLine; readTokenMetrics uses
// PromptEvalCount/EvalCount > 0 as its signal.
type ollamaMetrics struct {
	Done               bool  `json:"done"`
	PromptEvalCount    int   `json:"prompt_eval_count"`
	EvalCount          int   `json:"eval_count"`
	TotalDuration      int64 `json:"total_duration"`
	LoadDuration       int64 `json:"load_duration"`
	PromptEvalDuration int64 `json:"prompt_eval_duration"`
	EvalDuration       int64 `json:"eval_duration"`
}

// openaiUsage is the OpenAI response shape for token extraction. Shared by
// readTokenMetrics (non-streaming) and processLine (streaming SSE).
type openaiUsage struct {
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// readTokenMetrics reads a non-streaming 2xx response body, extracts token
// counts + durations, and re-wraps the body so the caller can still read it.
// Tries Ollama native format (prompt_eval_count, eval_count, *_duration)
// first, then OpenAI format (usage.prompt_tokens, usage.completion_tokens).
// Returns zero-value tokenMetrics if the body is nil, status is non-2xx, or
// the JSON has no recognized fields.
func readTokenMetrics(resp *http.Response, model string) tokenMetrics {
	if resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return tokenMetrics{Model: model}
	}
	body := drainBody(resp)
	if body == nil {
		return tokenMetrics{Model: model}
	}

	m := tokenMetrics{Model: model}
	var ollama ollamaMetrics
	if json.Unmarshal(body, &ollama) == nil && (ollama.PromptEvalCount > 0 || ollama.EvalCount > 0) {
		m.applyOllama(ollama)
		return m
	}
	var openai openaiUsage
	if json.Unmarshal(body, &openai) == nil && (openai.Usage.PromptTokens > 0 || openai.Usage.CompletionTokens > 0) {
		m.applyOpenAI(openai)
		return m
	}
	return m
}

// applyOllama copies Ollama-native token + duration fields into m.
// Shared by readTokenMetrics (non-streaming) and processLine (streaming).
func (m *tokenMetrics) applyOllama(o ollamaMetrics) {
	m.PromptTokens = o.PromptEvalCount
	m.OutputTokens = o.EvalCount
	m.TotalDuration = o.TotalDuration
	m.LoadDuration = o.LoadDuration
	m.PromptEvalDuration = o.PromptEvalDuration
	m.EvalDuration = o.EvalDuration
}

// applyOpenAI copies OpenAI usage token fields into m. Durations stay zero
// (OpenAI responses don't include them).
func (m *tokenMetrics) applyOpenAI(u openaiUsage) {
	m.PromptTokens = u.Usage.PromptTokens
	m.OutputTokens = u.Usage.CompletionTokens
}

// metricReader is a pass-through io.ReadCloser that observes NDJSON (Ollama)
// or SSE (OpenAI) streaming responses, extracting token metrics from the
// final chunk. Bytes are passed to the caller unchanged — the client gets
// the exact same stream. The onDone callback fires once when the stream
// ends (EOF or Close), regardless of whether metrics were found.
//
// idleTimeout: if no data arrives for this duration, the context is cancelled,
// which closes the upstream connection. timedOut is set so onDone knows to
// reclassify the event as a 502 instead of backfilling metrics.
type metricReader struct {
	src         io.ReadCloser
	metrics     *tokenMetrics
	buf         []byte // incomplete trailing line
	found       bool
	onDone      func()
	once        sync.Once
	cancel      context.CancelFunc
	timer       *time.Timer
	idleTimeout time.Duration
	timedOut    bool
}

func (r *metricReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 {
		r.scan(p[:n])
		if r.timer != nil {
			r.timer.Reset(r.idleTimeout)
		}
	}
	if err != nil {
		r.stopTimer()
		r.once.Do(r.onDone)
	}
	return n, err
}

func (r *metricReader) Close() error {
	r.stopTimer()
	r.once.Do(r.onDone)
	if r.cancel != nil {
		r.cancel()
	}
	return r.src.Close()
}

func (r *metricReader) stopTimer() {
	if r.timer != nil {
		r.timer.Stop()
	}
}

// scan processes complete lines in data, extracting metrics from the final
// chunk. Incomplete trailing data is buffered for the next call.
// ponytail: ceiling — buffer holds at most one incomplete line (a few KB
// for Ollama chat). If someone streams huge single-line JSON chunks, the
// buffer grows to that size. Upgrade to a bounded buffer + skip if
// line exceeds N bytes.
func (r *metricReader) scan(data []byte) {
	if r.found {
		return
	}
	r.buf = append(r.buf, data...)
	for {
		idx := bytes.IndexByte(r.buf, '\n')
		if idx < 0 {
			break
		}
		line := r.buf[:idx]
		r.buf = r.buf[idx+1:]
		r.processLine(bytes.TrimSpace(line))
	}
}

// processLine tries to extract metrics from one NDJSON/SSE line.
func (r *metricReader) processLine(line []byte) {
	if len(line) == 0 {
		return
	}
	// Ollama NDJSON: line is a JSON object. The final chunk has "done":true
	// and includes prompt_eval_count, eval_count, *_duration.
	// OpenAI SSE: line starts with "data: ". Skip "[DONE]". The final
	// chunk (if usage is included) has a "usage" object.
	payload := line
	if bytes.HasPrefix(line, []byte("data: ")) {
		payload = bytes.TrimSpace(line[6:])
		if bytes.Equal(payload, []byte("[DONE]")) {
			return
		}
	}

	var ollama ollamaMetrics
	if json.Unmarshal(payload, &ollama) == nil && ollama.Done {
		r.metrics.applyOllama(ollama)
		r.found = true
		return
	}

	var openai openaiUsage
	if json.Unmarshal(payload, &openai) == nil && (openai.Usage.PromptTokens > 0 || openai.Usage.CompletionTokens > 0) {
		r.metrics.applyOpenAI(openai)
		r.found = true
		return
	}
}

// ---------- server ----------

// Server is the allocate.d proxy. It owns the key pool, stats, and HTTP server.
// The daemon (allocate_d-server.exe) constructs one via NewServer and calls
// ListenAndServe. The Flutter app does not share this process — it consumes
// state over /stats and /events.
type Server struct {
	cfg          Config
	cfgMu        sync.RWMutex // guards cfg for hot-reload via ReloadConfig
	upstreamURL  *url.URL     // parsed cfg.Upstream, re-parsed on reload
	store        *Store
	poolMu       sync.RWMutex
	pool         *keyPool
	st           *stats
	bc           *broadcaster
	http         *http.Server
	inFlightMu   sync.Mutex
	inFlight     []InFlightEntry
	nextInFlight int64
	startTime    time.Time
	compMu       sync.RWMutex
	compression  CompressionSettings

	// UIHandler, if set, serves the embedded admin UI under /ui/.
	UIHandler http.Handler
}

// NewServer builds a Server from a Config and an open Store. The key pool
// is loaded from the store's first provider (Ollama-only today). If the
// store has no providers, the pool is empty and every request fails with
// 503 until keys are added via the admin endpoints.
func NewServer(cfg Config, store *Store) (*Server, error) {
	pool, err := loadPool(store)
	if err != nil {
		return nil, fmt.Errorf("load key pool: %w", err)
	}
	// Merge store-backed config: if the store has a "config" key, those
	// values override env-derived cfg. If not, seed the store with the env
	// values (one-shot — same lifecycle as ALLOCATED_KEYS).
	if store != nil {
		if val, _ := store.GetSetting("config"); val != "" {
			var sc Config
			if json.Unmarshal([]byte(val), &sc) == nil {
				cfg.Upstream = sc.Upstream
				cfg.Cooldown = sc.Cooldown
				cfg.Port = sc.Port
				cfg.KeySelection = sc.KeySelection
			}
		} else {
			seed := Config{Upstream: cfg.Upstream, Cooldown: cfg.Cooldown, Port: cfg.Port, KeySelection: ""}
			if b, err := json.Marshal(seed); err == nil {
				_ = store.SetSetting("config", string(b))
			}
		}
	}
	upstream, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}
	st := newStats(100)
	srv := &Server{cfg: cfg, upstreamURL: upstream, store: store, pool: pool, st: st, startTime: time.Now()}
	srv.bc = newBroadcaster()
	srv.compression = loadCompression(store)
	pool.selection = cfg.KeySelection // apply initial key selection mode
	// Background usage refresher: fetch each key's real Ollama quota usage
	// every minute so the "least_used" selection compares actual quota, not
	// proxy request counts. ponytail: ceiling — up to 1 min stale mid-burst;
	// the 429-retry-and-bench path covers the gap. Upgrade path: refresh the
	// benched key inline on 429 before retry.
	go func() {
		pool.refreshUsage(upstream.String())
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for range t.C {
			srv.poolMu.RLock()
			p := srv.pool
			srv.poolMu.RUnlock()
			if p == nil {
				continue
			}
			srv.cfgMu.RLock()
			u := srv.upstreamURL
			srv.cfgMu.RUnlock()
			if u != nil {
				p.refreshUsage(u.String())
			}
		}
	}()
	if err := srv.hydrateStats(); err != nil {
		log.Printf("warning: hydrate stats: %v (starting with zero counters)", err)
	}
	return srv, nil
}

// loadPool reads keys for the first provider from the store and builds a
// keyPool. Returns an empty pool (no error) if the store has no providers
// yet — the daemon stays up, requests just fail with 503 until keys are
// added. ponytail: first-provider-only — multi-provider routing is YAGNI
// until a second provider exists.
func loadPool(store *Store) (*keyPool, error) {
	if store == nil {
		return newKeyPool(nil, nil), nil
	}
	id, err := store.FirstProviderID()
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return newKeyPool(nil, store), nil
	}
	rows, err := store.ListKeys(id)
	if err != nil {
		return nil, err
	}
	return newKeyPool(rows, store), nil
}

// loadCompression reads compression settings from the store, falling back to
// defaults if the store is nil or the key is missing.
func loadCompression(store *Store) CompressionSettings {
	cfg := defaultCompression()
	if store == nil {
		return cfg
	}
	val, err := store.GetSetting("compression")
	if err != nil || val == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(val), &cfg); err != nil {
		return defaultCompression() // bad JSON — safe defaults
	}
	return cfg
}

// ReloadCompression re-reads compression settings from the store and updates
// the in-memory config. Called after admin PUT /admin/compression.
func (s *Server) ReloadCompression() {
	cfg := loadCompression(s.store)
	s.compMu.Lock()
	s.compression = cfg
	s.compMu.Unlock()
}

// compressionConfig returns a snapshot of the current compression settings
// under a read lock. Safe to call from RoundTrip hot path.
func (s *Server) compressionConfig() CompressionSettings {
	s.compMu.RLock()
	defer s.compMu.RUnlock()
	return s.compression
}

// ReloadConfig re-reads store-backed config (upstream, cooldown, port) and
// updates the in-memory cfg + re-parses the upstream URL. Called after
// admin PUT /admin/config. Port changes persist but only take effect on
// restart (the listener is already bound). Does NOT call notify() — config
// is not part of the Snapshot.
func (s *Server) ReloadConfig() {
	if s.store == nil {
		return
	}
	val, err := s.store.GetSetting("config")
	if err != nil || val == "" {
		return
	}
	var sc Config
	if json.Unmarshal([]byte(val), &sc) != nil {
		return
	}
	s.cfgMu.Lock()
	s.cfg.Upstream = sc.Upstream
	s.cfg.Cooldown = sc.Cooldown
	s.cfg.Timeout = sc.Timeout
	s.cfg.Port = sc.Port
	s.cfg.KeySelection = sc.KeySelection
	if u, err := url.Parse(sc.Upstream); err == nil {
		s.upstreamURL = u
	}
	// Propagate selection mode to the live pool.
	if sc.KeySelection == "least_used" || sc.KeySelection == "lru" {
		s.poolMu.Lock()
		if s.pool != nil {
			s.pool.selection = sc.KeySelection
		}
		s.poolMu.Unlock()
	}
	s.cfgMu.Unlock()
}

// configSnapshot returns a copy of the current config under a read lock.
// Used by /admin/config GET and /healthz.
func (s *Server) configSnapshot() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// cooldown returns the current cooldown duration under a read lock.
// Called by retryTransport on each Release.
func (s *Server) cooldown() time.Duration {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return time.Duration(s.cfg.Cooldown)
}

// timeout returns the streaming idle timeout from the config. If 0, idle
// timeout is disabled (requests can stall indefinitely).
func (s *Server) timeout() time.Duration {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return time.Duration(s.cfg.Timeout)
}

// ReloadPool rebuilds the in-memory key pool from the store. Called after
// admin add/remove key operations so the proxy picks up changes without
// a restart. Safe to call concurrently with Acquire/Release — the old
// pool is replaced atomically under the server's own lock.
func (s *Server) ReloadPool() error {
	pool, err := loadPool(s.store)
	if err != nil {
		return err
	}
	s.poolMu.Lock()
	s.pool = pool
	s.poolMu.Unlock()
	s.notify()
	return nil
}

// hydrateStats seeds the in-memory atomic counters + ringBuffer from the
// persisted event log on daemon startup, so the dashboard boots warm instead
// of showing zeros until new requests arrive. Non-fatal: on failure, the
// daemon starts with zero counters (current behavior).
func (s *Server) hydrateStats() error {
	if s.store == nil {
		return nil
	}
	total, ok, rate429, server5xx, retries, err := s.store.EventCounts()
	if err != nil {
		return err
	}
	s.st.total.Store(total)
	s.st.ok.Store(ok)
	s.st.rateLim.Store(rate429)
	s.st.server.Store(server5xx)
	s.st.retries.Store(retries)

	// Hydrate compression counters from persisted events.
	if compReq, saved, err := s.store.CompressionCounts(); err == nil {
		s.st.compressed.Store(compReq)
		s.st.saved.Store(saved)
	}

	// RecentEvents returns newest-first; Add() writes to the ring head,
	// so we insert oldest-first to end up with newest at the head.
	evs, err := s.store.RecentEvents(100)
	if err != nil {
		return err
	}
	s.poolMu.RLock()
	pool := s.pool
	s.poolMu.RUnlock()
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		keyIdx := -1
		if e.KeyID != nil {
			for j, ks := range pool.keys {
				if ks.id == *e.KeyID {
					keyIdx = j
					break
				}
			}
		}
		s.st.log.Add(ReqEntry{
			Time:               time.UnixMilli(e.TS),
			Key:                keyIdx,
			Method:             e.Method,
			Path:               e.Path,
			Status:             e.Status,
			Latency:            e.LatencyMs,
			Retried:            e.Retry == 1,
			ErrorMsg:           e.ErrorMsg,
			Model:              e.Model,
			PromptTokens:       e.PromptTokens,
			OutputTokens:       e.OutputTokens,
			TotalDuration:      e.TotalDuration,
			LoadDuration:       e.LoadDuration,
			PromptEvalDuration: e.PromptEvalDuration,
			EvalDuration:       e.EvalDuration,
			Compressed:         e.Compressed,
			TokensSaved:        e.TokensSaved,
		})
	}
	log.Printf("hydrated: total=%d, recent=%d", total, len(evs))
	return nil
}

// Snapshot returns the current dashboard state.
func (s *Server) Snapshot() Snapshot {
	s.poolMu.RLock()
	pool := s.pool
	s.poolMu.RUnlock()
	r := Snapshot{
		Keys:     pool.Snapshot(),
		Recent:   s.st.log.Snapshot(),
		InFlight: s.inFlightSnapshot(),
		Stats: StatsSnapshot{
			Total:          s.st.total.Load(),
			OK:             s.st.ok.Load(),
			Rate429:        s.st.rateLim.Load(),
			Server5xx:      s.st.server.Load(),
			Retries:        s.st.retries.Load(),
			CompressedReqs: s.st.compressed.Load(),
			TokensSaved:    s.st.saved.Load(),
			UpstreamErrors: s.st.upstreamErr.Load(),
		},
	}
	lastErr, lastAt := s.st.upstreamErrInfo()
	if lastErr != "" {
		r.Stats.LastUpstreamError = lastErr
		r.Stats.LastUpstreamErrorAt = float64(lastAt.UnixMilli()) / 1000
	}
	return r
}

// notify pushes the current snapshot to all SSE subscribers on /events.
func (s *Server) notify() {
	if s.bc == nil {
		return
	}
	snap := s.Snapshot()
	if b, err := json.Marshal(snap); err == nil {
		s.bc.Broadcast(b)
	}
}

// addInFlight records a newly-started request in the in-flight list and
// returns its monotonic ID. Called by retryTransport.RoundTrip right after
// Acquire succeeds, before exec(). The ID is used by the frontend as a
// stable v-for key and to match start/end events for the 300ms hold.
func (s *Server) addInFlight(method, path string, keyIdx int, model string) int64 {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	s.nextInFlight++
	id := s.nextInFlight
	s.inFlight = append(s.inFlight, InFlightEntry{
		ID:        id,
		Method:    method,
		Path:      path,
		KeyIndex:  keyIdx,
		Model:     model,
		StartedAt: float64(time.Now().UnixNano()) / 1e9,
	})
	return id
}

// removeInFlight removes a completed request from the in-flight list by ID.
// Called by retryTransport.RoundTrip at every exit path after the request
// completes.
func (s *Server) removeInFlight(id int64) {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	s.inFlight = slices.DeleteFunc(s.inFlight, func(e InFlightEntry) bool { return e.ID == id })
}

// inFlightSnapshot returns a copy of the current in-flight list for the
// Snapshot struct. Called from Snapshot().
func (s *Server) inFlightSnapshot() []InFlightEntry {
	s.inFlightMu.Lock()
	defer s.inFlightMu.Unlock()
	out := make([]InFlightEntry, len(s.inFlight))
	copy(out, s.inFlight)
	return out
}

// updateEntryMetrics backfills token metrics into a DB event row + the
// in-memory ring buffer after a streaming response completes. Called by
// metricReader.onDone. eventID is the DB row ID from record(). entryTime
// is the ReqEntry.Time (used to match the ring buffer entry). notify()
// pushes the updated snapshot to the dashboard.
func (s *Server) updateEntryMetrics(eventID int64, entryTime time.Time, m tokenMetrics) {
	if s.store != nil && eventID > 0 {
		_ = s.store.UpdateEventMetrics(eventID, m.Model, m.PromptTokens, m.OutputTokens,
			m.TotalDuration, m.LoadDuration, m.PromptEvalDuration, m.EvalDuration)
	}
	// Backfill tokens_saved in the ring buffer and atomic counter for
	// streaming responses. The DB row was updated by UpdateEventMetrics
	// (which computes tokens_saved from pre_bytes/post_bytes + prompt_tokens).
	if m.PreBytes > 0 && m.PostBytes > 0 && m.PostBytes < m.PreBytes && m.PromptTokens > 0 {
		estOriginal := int64(m.PreBytes) / 4
		saved := estOriginal - int64(m.PromptTokens)
		if saved < 0 {
			saved = 0
		}
		s.st.saved.Add(saved - m.TokensSaved) // add the delta (m.TokensSaved was 0 at record time)
		s.st.log.UpdateMetrics(entryTime, tokenMetrics{
			Model:        m.Model,
			PromptTokens: m.PromptTokens,
			OutputTokens: m.OutputTokens,
			TokensSaved:  saved,
		}, false, 0, "")
	} else {
		s.st.log.UpdateMetrics(entryTime, m, false, 0, "")
	}
	s.notify()
}

// ListenAndServe runs the proxy on cfg.Port. Blocks until Shutdown.
// Upstream and cooldown are read live from cfg (hot-reloadable via
// ReloadConfig). Port is bound once here and cannot change without restart.
func (s *Server) ListenAndServe(debug bool) error {
	logf := func(format string, args ...any) {
		if debug {
			log.Printf(format, args...)
		}
	}
	transport := newRetryTransport(logf, s.st, s)

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			s.cfgMu.RLock()
			u := s.upstreamURL
			s.cfgMu.RUnlock()
			req.URL.Scheme = u.Scheme
			req.URL.Host = u.Host
			req.Host = u.Host
			// Authorization is set by the transport, not here.
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, `{"error":"upstream unreachable"}`, http.StatusBadGateway)
			s.st.recordUpstreamError(err)
			s.notify()
			log.Printf("upstream error: %v", err)
		},
	}

	// /stats, /events, /healthz are allocate.d paths; everything else forwards to upstream.
	// /admin/* paths mutate the store (providers, keys) and are localhost-only
	// (no auth — single-user assumption, same as the proxy itself).
	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/stats":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.Snapshot())
		case r.URL.Path == "/events":
			s.serveEvents(w, r)
		case r.URL.Path == "/healthz":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.HealthInfo())
		case strings.HasPrefix(r.URL.Path, "/admin/"):
			s.serveAdmin(w, r)
		case r.URL.Path == "/":
			if s.UIHandler != nil {
				s.UIHandler.ServeHTTP(w, r)
			} else {
				rp.ServeHTTP(w, r)
			}
		default:
			rp.ServeHTTP(w, r)
		}
	})

	cfg := s.configSnapshot()
	addr := ":" + cfg.Port
	s.poolMu.RLock()
	keyCount := len(s.pool.keys)
	s.poolMu.RUnlock()
	log.Printf("allocate.d listening on %s, upstream=%s, keys=%d, cooldown=%s",
		addr, cfg.Upstream, keyCount, time.Duration(cfg.Cooldown))
	s.http = &http.Server{
		Addr:              addr,
		Handler:           rootHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s.http.ListenAndServe()
}

// Shutdown stops the HTTP server gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// serveEvents streams SSE updates to one client. Each client gets an
// immediate snapshot on connect, then a message on every state change.
// Drops on slow clients (16-msg buffer, non-blocking send).
func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming unsupported"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Initial snapshot: marshal once, hand to Subscribe so the client sees
	// current state without waiting for the next state change.
	var initial []byte
	if snap, err := json.Marshal(s.Snapshot()); err == nil {
		initial = snap
	}
	ch := s.bc.Subscribe(initial)
	defer s.bc.Unsubscribe(ch)

	// Initial snapshot already queued by Subscribe — flush it.
	if msg := <-ch; msg != nil {
		fmt.Fprintf(w, "data: %s\n\n", msg)
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

// ---------- env helpers ----------

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envDurOr(key string, def time.Duration) time.Duration {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// ---------- admin endpoints ----------
//
// /admin/providers  GET    — list providers
// /admin/providers  POST   — add provider {name, upstream}
// /admin/providers/{id} DELETE — remove provider (cascades to keys)
// /admin/keys       GET    — list keys for ?provider_id= (default: first)
// /admin/keys       POST   — add key {provider_id, plaintext, name}
// /admin/keys/{id}  DELETE — remove key
// /admin/keys/{id}  PATCH  — rename key {name}
// /admin/events     GET    — recent events ?limit= (default 100, newest first)
// /admin/compression GET   — compression config
// /admin/compression PUT   — update compression config
// /admin/metrics    GET    — aggregated metrics ?hours= (default 0=all-time)
//
// No auth — single-user localhost assumption (same as the proxy itself).
// After any mutation, the server reloads the key pool so the proxy picks
// up changes without a restart.

func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/admin/")

	if path == "providers" {
		switch r.Method {
		case http.MethodGet:
			provs, err := s.store.ListProviders()
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			if provs == nil {
				provs = []Provider{}
			}
			json.NewEncoder(w).Encode(provs)
		case http.MethodPost:
			var body struct {
				Name     string `json:"name"`
				Upstream string `json:"upstream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			if body.Name == "" || body.Upstream == "" {
				http.Error(w, `{"error":"name and upstream required"}`, http.StatusBadRequest)
				return
			}
			if _, err := s.store.AddProvider(body.Name, body.Upstream); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			_ = s.ReloadPool()
			w.WriteHeader(http.StatusCreated)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if strings.HasPrefix(path, "providers/") {
		id, err := strconv.ParseInt(strings.TrimPrefix(path, "providers/"), 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if r.Method != http.MethodDelete {
			methodNotAllowed(w)
			return
		}
		if err := s.store.RemoveProvider(id); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		_ = s.ReloadPool()
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if path == "keys" {
		switch r.Method {
		case http.MethodGet:
			pid := s.firstProviderID()
			if v := r.URL.Query().Get("provider_id"); v != "" {
				if id, err := strconv.ParseInt(v, 10, 64); err == nil {
					pid = id
				}
			}
			rows, err := s.store.ListKeys(pid)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			for i := range rows {
				rows[i].Plaintext = ""
			}
			if rows == nil {
				rows = []KeyRow{}
			}
			json.NewEncoder(w).Encode(rows)
		case http.MethodPost:
			var body struct {
				ProviderID int64  `json:"provider_id"`
				Plaintext  string `json:"plaintext"`
				Name       string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			if body.Plaintext == "" {
				http.Error(w, `{"error":"plaintext required"}`, http.StatusBadRequest)
				return
			}
			if body.ProviderID == 0 {
				body.ProviderID = s.firstProviderID()
			}
			if _, err := s.store.AddKey(body.ProviderID, body.Plaintext, body.Name); err != nil {
				writeKeyNameErr(w, err)
				return
			}
			_ = s.ReloadPool()
			w.WriteHeader(http.StatusCreated)
		default:
			methodNotAllowed(w)
		}
		return
	}
	if strings.HasPrefix(path, "keys/") {
		// Check for keys/{id}/test sub-path before parsing the ID.
		rest := strings.TrimPrefix(path, "keys/")
		if strings.HasSuffix(rest, "/test") {
			idStr := strings.TrimSuffix(rest, "/test")
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			if r.Method != http.MethodGet {
				methodNotAllowed(w)
				return
			}
			json.NewEncoder(w).Encode(s.TestKey(id))
			return
		}
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		switch r.Method {
		case http.MethodDelete:
			if err := s.store.RemoveKey(id); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			_ = s.ReloadPool()
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPatch:
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			if err := s.store.RenameKey(id, body.Name); err != nil {
				writeKeyNameErr(w, err)
				return
			}
			_ = s.ReloadPool()
			w.WriteHeader(http.StatusNoContent)
		default:
			methodNotAllowed(w)
		}
		return
	}

	if path == "events" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				limit = int(n)
			}
		}
		evs, err := s.store.RecentEvents(limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if evs == nil {
			evs = []Event{}
		}
		json.NewEncoder(w).Encode(evs)
		return
	}

	if path == "metrics" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		hours := 0
		if v := r.URL.Query().Get("hours"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				hours = int(n)
			}
		}
		mm, err := s.store.Metrics(hours)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		json.NewEncoder(w).Encode(mm)
		return
	}

	if path == "config" {
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(s.configSnapshot())
		case http.MethodPut:
			var sc Config
			if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			if sc.Upstream == "" || sc.Port == "" {
				http.Error(w, `{"error":"upstream and port required"}`, http.StatusBadRequest)
				return
			}
			if _, err := url.Parse(sc.Upstream); err != nil {
				http.Error(w, `{"error":"invalid upstream URL"}`, http.StatusBadRequest)
				return
			}
			if time.Duration(sc.Cooldown) < 0 {
				http.Error(w, `{"error":"cooldown must be non-negative"}`, http.StatusBadRequest)
				return
			}
			if time.Duration(sc.Timeout) < 0 {
				http.Error(w, `{"error":"timeout must be non-negative"}`, http.StatusBadRequest)
				return
			}
			b, _ := json.Marshal(sc)
			if err := s.store.SetSetting("config", string(b)); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			s.ReloadConfig()
			w.WriteHeader(http.StatusNoContent)
		default:
			methodNotAllowed(w)
		}
		return
	}

	if path == "compression" {
		switch r.Method {
		case http.MethodGet:
			cfg := s.compressionConfig()
			json.NewEncoder(w).Encode(cfg)
		case http.MethodPut:
			var cfg CompressionSettings
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			b, _ := json.Marshal(cfg)
			if err := s.store.SetSetting("compression", string(b)); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			s.ReloadCompression()
			s.notify()
			w.WriteHeader(http.StatusNoContent)
		default:
			methodNotAllowed(w)
		}
		return
	}

	if path == "probe" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		json.NewEncoder(w).Encode(s.ProbeUpstream())
		return
	}

	if path == "usage" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.cfgMu.RLock()
		u := s.upstreamURL
		s.cfgMu.RUnlock()
		if u == nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("no upstream configured"))
			return
		}
		pid := s.firstProviderID()
		rows, err := s.store.ListKeys(pid)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		results := make([]KeyUsageResult, len(rows))
		var wg sync.WaitGroup
		for i, row := range rows {
			wg.Add(1)
			go func(idx int, r KeyRow) {
				defer wg.Done()
				results[idx] = KeyUsageResult{
					KeyID:   r.ID,
					KeyName: r.Name,
				}
				usage, err := fetchUsage(u.String(), r.Plaintext, 10*time.Second)
				if err != nil {
					results[idx].Error = err.Error()
					return
				}
				results[idx].OK = true
				results[idx].Usage = usage
			}(i, row)
		}
		wg.Wait()
		json.NewEncoder(w).Encode(results)
		return
	}

	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

// firstProviderID returns the DB row ID of the first provider, or 0 if none.
// Used as the default provider_id for key operations (Ollama-only today).
func (s *Server) firstProviderID() int64 {
	id, err := s.store.FirstProviderID()
	if err != nil {
		return 0
	}
	return id
}

func writeErr(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": err.Error()})
	w.Write(b)
}

// writeKeyNameErr maps store-level key-name errors to HTTP responses.
// Shared by the AddKey and RenameKey admin handlers.
func writeKeyNameErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrKeyNameEmpty):
		http.Error(w, `{"error":"name required"}`, http.StatusBadRequest)
	case errors.Is(err, ErrKeyNameTaken):
		http.Error(w, `{"error":"name already in use"}`, http.StatusConflict)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// methodNotAllowed writes the standard 405 response for unsupported admin methods.
func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
}
