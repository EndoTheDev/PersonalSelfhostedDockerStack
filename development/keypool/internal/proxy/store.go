package proxy

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed persistence layer for allocate.d: providers, keys
// (encrypted at rest), and the request event log. WAL mode, single-writer.
type Store struct {
	db        *sql.DB
	masterKey []byte // 32 bytes, never persisted to DB
}

// Provider is one upstream provider row.
type Provider struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Upstream string `json:"upstream"`
}

// KeyRow is one API key as returned by ListKeys — plaintext is decrypted
// only inside the proxy; the dashboard never sees it.
type KeyRow struct {
	ID         int64     `json:"id"`
	ProviderID int64     `json:"provider_id"`
	Name       string    `json:"name"`              // display label, unique per provider ("" = grandfathered)
	Plaintext  string    `json:"-"`                 // never JSON-marshaled out
	UseCount   int64     `json:"use_count"`
	CoolUntil  *time.Time `json:"cool_until"`        // nil = available
	LastUsed   *time.Time `json:"last_used"`         // nil = never used
	AddedAt    time.Time `json:"added_at"`
}

// Event is one row in the request log.
type Event struct {
	ID            int64  `json:"id"`
	TS            int64  `json:"ts"`            // unix millis
	KeyID         *int64 `json:"key_id"`        // nil for all-keys-cooling events
	Method        string `json:"method"`
	Path          string `json:"path"`
	Status        int    `json:"status"`
	LatencyMs     int64  `json:"latency_ms"`
	Retry         int    `json:"retry"`
	ErrorMsg      string `json:"error_msg,omitempty"`       // extracted from upstream {"error":"..."} body
	Model         string `json:"model,omitempty"`            // model name from request body
	PromptTokens  int    `json:"prompt_tokens,omitempty"`   // input tokens (prompt_eval_count / usage.prompt_tokens)
	OutputTokens  int    `json:"output_tokens,omitempty"`   // output tokens (eval_count / usage.completion_tokens)
	TotalDuration int64  `json:"total_duration,omitempty"`  // nanoseconds (raw from Ollama)
	LoadDuration  int64  `json:"load_duration,omitempty"`
	PromptEvalDuration int64 `json:"prompt_eval_duration,omitempty"`
	EvalDuration  int64  `json:"eval_duration,omitempty"`
	Compressed    bool   `json:"compressed"`               // true if request body was compressed
	PreBytes      int    `json:"pre_bytes,omitempty"`      // body bytes before compression
	PostBytes     int    `json:"post_bytes,omitempty"`     // body bytes after compression
	TokensSaved   int64  `json:"tokens_saved,omitempty"`   // estimated tokens saved by compression
}

// OpenStore opens (or creates) the SQLite database at path, enables WAL,
// runs schema migrations, and returns a Store. masterKey is the 32-byte
// AES-256 key used to encrypt/decrypt API keys at rest.
func OpenStore(path string, masterKey []byte) (*Store, error) {
	// modernc.org/sqlite uses the "sqlite" driver name. busy_timeout lets
	// concurrent readers wait briefly instead of failing on lock contention.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single writer connection — modernc.org/sqlite serializes writes anyway,
	// and this avoids "database is locked" errors from interleaved txns.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db, masterKey: masterKey}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS providers (
  id        INTEGER PRIMARY KEY,
  name      TEXT NOT NULL UNIQUE,
  upstream  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS keys (
  id          INTEGER PRIMARY KEY,
  provider_id INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
  ciphertext  BLOB NOT NULL,
  nonce       BLOB NOT NULL,
  use_count   INTEGER NOT NULL DEFAULT 0,
  cool_until  INTEGER,
  added_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  id         INTEGER PRIMARY KEY,
  ts         INTEGER NOT NULL,
  key_id     INTEGER,
  method     TEXT,
  path       TEXT,
  status     INTEGER,
  latency_ms INTEGER,
  retry      INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// ponytail: ceiling — try/ignore works because we add columns once and
	// never rename/drop. If migrations get complex, add a schema_version table.
	if _, err := s.db.Exec(`ALTER TABLE keys ADD COLUMN name TEXT NOT NULL DEFAULT ''`); err != nil {
		if !isDupColErr(err) {
			return err
		}
	}
	if _, err := s.db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_keys_name ON keys(provider_id, name) WHERE name != ''`,
	); err != nil {
		return err
	}
	if _, err := s.db.Exec(`ALTER TABLE events ADD COLUMN error_msg TEXT NOT NULL DEFAULT ''`); err != nil {
		if !isDupColErr(err) {
			return err
		}
	}
	// ponytail: ceiling — 7 separate ALTER TABLEs; if migrations get complex,
	// add a schema_version table and batch these.
	metricCols := []string{
		`ALTER TABLE events ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE events ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN total_duration INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN load_duration INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN prompt_eval_duration INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN eval_duration INTEGER NOT NULL DEFAULT 0`,
	}
	for _, q := range metricCols {
		if _, err := s.db.Exec(q); err != nil {
			if !isDupColErr(err) {
				return err
			}
		}
	}
	// Compression tracking columns — added for lite compression ROI.
	// ponytail: ceiling — tokens_saved is a char-count estimate (pre_bytes/4
	// minus real prompt_tokens), not a real tokenizer count. CHARS_PER_TOKEN=4
	// is the same constant OmniRoute uses. Upgrade path: real tokenizer on
	// the pre-compression body (Ollama PR #12030, unmerged).
	compressionCols := []string{
		`ALTER TABLE events ADD COLUMN compressed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN pre_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN post_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE events ADD COLUMN tokens_saved INTEGER NOT NULL DEFAULT 0`,
	}
	for _, q := range compressionCols {
		if _, err := s.db.Exec(q); err != nil {
			if !isDupColErr(err) {
				return err
			}
		}
	}
	// last_used tracking — when a key was last acquired by the pool.
	if _, err := s.db.Exec(`ALTER TABLE keys ADD COLUMN last_used INTEGER NOT NULL DEFAULT 0`); err != nil {
		if !isDupColErr(err) {
			return err
		}
	}
	return nil
}

func isDupColErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

// ---------- settings ----------

// GetSetting reads a setting value by key. Returns "" and nil error if the
// key does not exist (caller applies defaults).
func (s *Store) GetSetting(key string) (string, error) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

// SetSetting upserts a setting value by key.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, key, value)
	return err
}

// ---------- providers ----------

func (s *Store) ListProviders() ([]Provider, error) {
	rows, err := s.db.Query(`SELECT id, name, upstream FROM providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, func(r *sql.Rows) (Provider, error) {
		var p Provider
		return p, r.Scan(&p.ID, &p.Name, &p.Upstream)
	})
}

// AddProvider inserts a new provider. Returns the inserted row ID.
func (s *Store) AddProvider(name, upstream string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO providers (name, upstream) VALUES (?, ?)`, name, upstream)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) RemoveProvider(id int64) error {
	_, err := s.db.Exec(`DELETE FROM providers WHERE id = ?`, id)
	return err
}

// FirstProviderID returns the DB row ID of the first provider (ORDER BY id),
// or 0 if none. Cheaper than ListProviders for callers that only need the ID.
func (s *Store) FirstProviderID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM providers ORDER BY id LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// ---------- keys ----------

// ListKeys returns all keys for a provider, with plaintext decrypted.
// Used by the proxy at startup and when the pool is refreshed after
// add/remove. The dashboard should NOT call this — it leaks plaintext.
func (s *Store) ListKeys(providerID int64) ([]KeyRow, error) {
	rows, err := s.db.Query(
		`SELECT id, provider_id, name, ciphertext, nonce, use_count, cool_until, last_used, added_at
		 FROM keys WHERE provider_id = ? ORDER BY id`, providerID)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, func(r *sql.Rows) (KeyRow, error) { return s.scanKey(r) })
}

// scanner is the common interface between *sql.Rows and *sql.Row — both
// implement Scan(dest ...any) error. Lets scanKey serve ListKeys and GetKey.
type scanner interface {
	Scan(dest ...any) error
}

// scanKey scans one key row (columns must match the ListKeys SELECT order),
// decrypts the plaintext, and hydrates the timestamp fields. Shared by
// ListKeys (via scanRows) and GetKey (single QueryRow).
func (s *Store) scanKey(r scanner) (KeyRow, error) {
	var k KeyRow
	var ct, nonce []byte
	var coolUntil, lastUsed sql.NullInt64
	var addedAt int64
	if err := r.Scan(&k.ID, &k.ProviderID, &k.Name, &ct, &nonce, &k.UseCount, &coolUntil, &lastUsed, &addedAt); err != nil {
		return k, err
	}
	plain, err := Decrypt(s.masterKey, ct, nonce)
	if err != nil {
		return k, fmt.Errorf("decrypt key id=%d: %w", k.ID, err)
	}
	k.Plaintext = string(plain)
	k.AddedAt = time.Unix(addedAt, 0)
	if coolUntil.Valid {
		t := time.Unix(coolUntil.Int64, 0)
		k.CoolUntil = &t
	}
	if lastUsed.Valid && lastUsed.Int64 > 0 {
		t := time.Unix(lastUsed.Int64, 0)
		k.LastUsed = &t
	}
	return k, nil
}

// GetKey returns a single key by ID with plaintext decrypted. Used by the
// key-test endpoint to send a probe request with the key's bearer token.
func (s *Store) GetKey(id int64) (KeyRow, error) {
	row := s.db.QueryRow(
		`SELECT id, provider_id, name, ciphertext, nonce, use_count, cool_until, last_used, added_at
		 FROM keys WHERE id = ?`, id)
	return s.scanKey(row)
}

// ErrKeyNameEmpty is returned by AddKey/RenameKey when the name is empty
// after trimming. Existing grandfathered keys keep "" — this only blocks
// new writes.
var ErrKeyNameEmpty = errors.New("key name is required")

// ErrKeyNameTaken is returned by AddKey/RenameKey when the (non-empty) name
// is already used by another key in the same provider. Surfaced as 409 by
// the admin handlers.
var ErrKeyNameTaken = errors.New("key name already in use")

// MaxKeyNameLen is the enforced upper bound on key display names. Plenty
// for a label; prevents UI overflow and DB bloat.
const MaxKeyNameLen = 64

// validateName trims, checks non-empty, and clamps to MaxKeyNameLen.
// Shared by AddKey and RenameKey.
func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrKeyNameEmpty
	}
	if len(name) > MaxKeyNameLen {
		name = name[:MaxKeyNameLen]
	}
	return name, nil
}

// AddKey encrypts plaintext and inserts a new key row for providerID.
// name must be non-empty (after trim) and unique within the provider;
// otherwise ErrKeyNameEmpty / ErrKeyNameTaken is returned.
func (s *Store) AddKey(providerID int64, plaintext, name string) (int64, error) {
	name, err := validateName(name)
	if err != nil {
		return 0, err
	}
	return s.addKeyRaw(providerID, plaintext, name)
}

// addKeyRaw inserts a key without name validation. Used by AddKey (after
// validation) and SeedIfEmpty (grandfathered "" names for env-seeded keys).
func (s *Store) addKeyRaw(providerID int64, plaintext, name string) (int64, error) {
	ct, nonce, err := Encrypt(s.masterKey, []byte(plaintext))
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(
		`INSERT INTO keys (provider_id, name, ciphertext, nonce, use_count, cool_until, added_at)
		 VALUES (?, ?, ?, ?, 0, NULL, ?)`,
		providerID, name, ct, nonce, time.Now().Unix())
	if err != nil {
		if isUniqueErr(err) {
			return 0, ErrKeyNameTaken
		}
		return 0, err
	}
	return res.LastInsertId()
}

// RenameKey assigns a new display name to an existing key. name must be
// non-empty (after trim) and unique within the key's provider. Renaming a
// grandfathered (""-named) key to a real name is the intended upgrade path.
func (s *Store) RenameKey(id int64, name string) error {
	name, err := validateName(name)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE keys SET name = ? WHERE id = ?`, name, id)
	if err != nil && isUniqueErr(err) {
		return ErrKeyNameTaken
	}
	return err
}

// isUniqueErr reports whether err is a SQLite UNIQUE-constraint violation.
// modernc.org/sqlite surfaces these with "UNIQUE constraint failed" text.
func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *Store) RemoveKey(id int64) error {
	_, err := s.db.Exec(`DELETE FROM keys WHERE id = ?`, id)
	return err
}

// UpdateKeyState persists use_count, cool_until, and last_used for a key.
// Called by the keyPool after each request so the DB survives restarts.
// ponytail: write per request — fine at allocate.d's load (chat calls are
// seconds-long). If RPS ever makes this hot, batch updates on a ticker.
func (s *Store) UpdateKeyState(id int64, useCount int64, coolUntil *time.Time, lastUsed time.Time) error {
	var cool sql.NullInt64
	if coolUntil != nil {
		cool.Int64 = coolUntil.Unix()
		cool.Valid = true
	}
	_, err := s.db.Exec(`UPDATE keys SET use_count = ?, cool_until = ?, last_used = ? WHERE id = ?`,
		useCount, cool, lastUsed.Unix(), id)
	return err
}

// ---------- events ----------

// InsertEvent appends one request event to the log. keyID is nil for
// all-keys-cooling failures (no key was acquired). Returns the event row ID
// — needed for streaming responses where token metrics are backfilled by
// UpdateEventMetrics after the stream completes.
func (s *Store) InsertEvent(e Event) (int64, error) {
	var keyID any
	if e.KeyID != nil {
		keyID = *e.KeyID
	}
	compressed := 0
	if e.Compressed {
		compressed = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO events (ts, key_id, method, path, status, latency_ms, retry, error_msg,
		   model, prompt_tokens, output_tokens, total_duration, load_duration, prompt_eval_duration, eval_duration,
		   compressed, pre_bytes, post_bytes, tokens_saved)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.TS, keyID, e.Method, e.Path, e.Status, e.LatencyMs, e.Retry, e.ErrorMsg,
		e.Model, e.PromptTokens, e.OutputTokens, e.TotalDuration, e.LoadDuration,
		e.PromptEvalDuration, e.EvalDuration,
		compressed, e.PreBytes, e.PostBytes, e.TokensSaved)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateEventMetrics backfills token metrics for a streaming response after
// the stream completes and the final chunk (with done:true + durations) has
// been observed by the metricReader. Also computes tokens_saved from the
// byte ratio when compression was applied.
func (s *Store) UpdateEventMetrics(id int64, model string, promptTokens, outputTokens int, totalDuration, loadDuration, promptEvalDuration, evalDuration int64) error {
	_, err := s.db.Exec(
		`UPDATE events SET model = ?, prompt_tokens = ?, output_tokens = ?,
		   total_duration = ?, load_duration = ?, prompt_eval_duration = ?, eval_duration = ?
		 WHERE id = ?`,
		model, promptTokens, outputTokens, totalDuration, loadDuration,
		promptEvalDuration, evalDuration, id)
	if err != nil {
		return err
	}
	// Backfill tokens_saved: char-count estimate (pre_bytes/4) minus real
	// prompt_tokens from the upstream response.
	// ponytail: ceiling — pre_bytes/4 is a rough token estimate (CHARS_PER_TOKEN=4,
	// same constant OmniRoute uses). Post side is real prompt_tokens. Upgrade
	// path: real tokenizer on the pre-compression body (Ollama PR #12030, unmerged).
	_, err = s.db.Exec(
		`UPDATE events SET tokens_saved = CASE
		   WHEN post_bytes > 0 AND post_bytes < pre_bytes AND prompt_tokens > 0
		     AND (pre_bytes / 4) > prompt_tokens
		     THEN CAST((pre_bytes / 4) - prompt_tokens AS INTEGER)
		   ELSE 0
		 END WHERE id = ?`, id)
	return err
}

// UpdateEventStatus reclassifies an event's status and error message after
// the fact. Used when a streaming response times out mid-stream — the event
// was recorded as 200, then reclassified to 502 on stream idle timeout.
func (s *Store) UpdateEventStatus(id int64, status int, errorMsg string) error {
	_, err := s.db.Exec(
		`UPDATE events SET status = ?, error_msg = ? WHERE id = ?`,
		status, errorMsg, id)
	return err
}

// RecentEvents returns up to limit events, newest first.
func (s *Store) RecentEvents(limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, ts, key_id, method, path, status, latency_ms, retry, error_msg,
		   model, prompt_tokens, output_tokens, total_duration, load_duration, prompt_eval_duration, eval_duration,
		   compressed, pre_bytes, post_bytes, tokens_saved
		 FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanRows(rows, func(r *sql.Rows) (Event, error) {
		var e Event
		var keyID sql.NullInt64
		var compressed int
		if err := r.Scan(&e.ID, &e.TS, &keyID, &e.Method, &e.Path, &e.Status, &e.LatencyMs, &e.Retry, &e.ErrorMsg,
			&e.Model, &e.PromptTokens, &e.OutputTokens, &e.TotalDuration, &e.LoadDuration,
			&e.PromptEvalDuration, &e.EvalDuration,
			&compressed, &e.PreBytes, &e.PostBytes, &e.TokensSaved); err != nil {
			return e, err
		}
		e.Compressed = compressed == 1
		if keyID.Valid {
			id := keyID.Int64
			e.KeyID = &id
		}
		return e, nil
	})
}

// EventCounts returns aggregate request counts from the events table for
// dashboard hydration on daemon startup. Single query, single round-trip.
func (s *Store) EventCounts() (total, ok, rate429, server5xx, retries int64, err error) {
	err = s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 200 AND status < 300 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status = 429 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN retry = 1 THEN 1 ELSE 0 END), 0)
		FROM events`).Scan(&total, &ok, &rate429, &server5xx, &retries)
	return
}

// CompressionCounts returns aggregate compression stats for dashboard
// hydration on daemon startup.
func (s *Store) CompressionCounts() (compressedReq, tokensSaved int64, err error) {
	err = s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN compressed = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(tokens_saved), 0)
		FROM events`).Scan(&compressedReq, &tokensSaved)
	return
}

// SeedIfEmpty is the one-shot migration path: if no providers exist and
// envKeys is non-empty, insert the default "ollama" provider and encrypt
// each env key into the keys table. Returns the number of keys migrated.
func (s *Store) SeedIfEmpty(providerName, upstream string, envKeys []string) (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM providers`).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 || len(envKeys) == 0 {
		return 0, nil
	}
	pid, err := s.AddProvider(providerName, upstream)
	if err != nil {
		return 0, err
	}
	for _, k := range envKeys {
		if _, err := s.addKeyRaw(pid, k, ""); err != nil {
			return 0, err
		}
	}
	return len(envKeys), nil
}

// ---------- metrics ----------

// Metrics is the aggregated data for the metrics dashboard. All slices are
// non-nil (empty [] not null) so JSON marshals cleanly for the frontend.
type Metrics struct {
	Totals      MetricsTotals      `json:"totals"`
	Statuses    []MetricsStatus    `json:"statuses"`
	ByModel     []MetricsByModel   `json:"by_model"`
	ByKey       []MetricsByKey     `json:"by_key"`
	ByDay       []MetricsByDay     `json:"by_day"`
	Compression CompressionTotals  `json:"compression"`
}

type MetricsTotals struct {
	Requests     int64   `json:"requests"`
	PromptTokens int64   `json:"prompt_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	ErrorRate    float64 `json:"error_rate"`
}

type MetricsStatus struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

type MetricsByModel struct {
	Model        string  `json:"model"`
	Count        int64   `json:"count"`
	PromptTokens int64   `json:"prompt_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
}

type MetricsByKey struct {
	KeyID        int64   `json:"key_id"`
	KeyName      string  `json:"key_name"`
	Count        int64   `json:"count"`
	PromptTokens int64   `json:"prompt_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
}

type MetricsByDay struct {
	Bucket       string `json:"bucket"`
	Count        int64  `json:"count"`
	PromptTokens int64  `json:"prompt_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TokensSaved  int64  `json:"tokens_saved"`
}

// CompressionTotals is the aggregate compression ROI block in Metrics.
type CompressionTotals struct {
	CompressedRequests int64 `json:"compressed_requests"`
	TokensSaved         int64 `json:"tokens_saved"`
	PromptTokens        int64 `json:"prompt_tokens"`
	PreBytes            int64 `json:"pre_bytes"`
	PostBytes           int64 `json:"post_bytes"`
}

// scanRows iterates rows, calling scan for each row, and appending the
// result to a slice. Closes rows on error and after the loop. Returns
// the collected slice or the first scan/query error.
// ponytail: ceiling — each Metrics query scans a different struct, so
// the scan callback handles the per-type field mapping.
func scanRows[T any](rows *sql.Rows, scan func(*sql.Rows) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Metrics returns aggregated event data for the metrics dashboard. hours
// controls the time window for all queries (0 = all-time). Bucket size for
// by_day adapts: minute for ≤1h, hourly for ≤24h, daily otherwise.
func (s *Store) Metrics(hours int) (*Metrics, error) {
	m := &Metrics{
		Statuses: []MetricsStatus{},
		ByModel:  []MetricsByModel{},
		ByKey:    []MetricsByKey{},
		ByDay:    []MetricsByDay{},
	}

	// Time filter: hours > 0 limits to the last N hours; 0 = all-time
	// (ts >= 0 matches all rows). ponytail: trivial filter on a small table.
	since := int64(0)
	if hours > 0 {
		since = time.Now().Add(-time.Duration(hours) * time.Hour).UnixMilli()
	}

	// Bucket format for by_day: minutes for ≤1h, hourly for ≤24h, daily otherwise.
	bucketFmt := "'%Y-%m-%d'"
	switch {
	case hours <= 1:
		bucketFmt = "'%Y-%m-%d %H:%M'"
	case hours <= 24:
		bucketFmt = "'%Y-%m-%d %H:00'"
	}

	// Totals — single row, single round-trip.
	if err := s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(AVG(latency_ms), 0),
		CASE WHEN COUNT(*) > 0 THEN CAST(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END) AS REAL) / COUNT(*) ELSE 0 END
		FROM events WHERE ts >= ?`, since).Scan(
		&m.Totals.Requests, &m.Totals.PromptTokens, &m.Totals.OutputTokens,
		&m.Totals.AvgLatencyMs, &m.Totals.ErrorRate,
	); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`SELECT
		CASE
		  WHEN status >= 200 AND status < 300 THEN '2xx'
		  WHEN status >= 300 AND status < 400 THEN '3xx'
		  WHEN status = 429 THEN '429'
		  WHEN status >= 400 AND status < 500 THEN '4xx'
		  WHEN status >= 500 THEN '5xx'
		  ELSE 'other'
		END, COUNT(*)
		FROM events WHERE ts >= ? GROUP BY 1 ORDER BY 2 DESC`, since)
	if err != nil {
		return nil, err
	}
	if m.Statuses, err = scanRows(rows, func(r *sql.Rows) (MetricsStatus, error) {
		var st MetricsStatus
		return st, r.Scan(&st.Label, &st.Count)
	}); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(`SELECT model, COUNT(*),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(output_tokens), 0),
		COALESCE(AVG(latency_ms), 0)
		FROM events WHERE model != '' AND ts >= ? GROUP BY model ORDER BY COUNT(*) DESC`, since)
	if err != nil {
		return nil, err
	}
	if m.ByModel, err = scanRows(rows, func(r *sql.Rows) (MetricsByModel, error) {
		var bm MetricsByModel
		return bm, r.Scan(&bm.Model, &bm.Count, &bm.PromptTokens, &bm.OutputTokens, &bm.AvgLatencyMs)
	}); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(`SELECT e.key_id,
		COALESCE(NULLIF(k.name, ''), 'key ' || e.key_id),
		COUNT(*), COALESCE(SUM(e.prompt_tokens), 0), COALESCE(SUM(e.output_tokens), 0),
		COALESCE(AVG(e.latency_ms), 0)
		FROM events e LEFT JOIN keys k ON e.key_id = k.id
		WHERE e.key_id IS NOT NULL AND e.ts >= ?
		GROUP BY e.key_id ORDER BY COUNT(*) DESC`, since)
	if err != nil {
		return nil, err
	}
	if m.ByKey, err = scanRows(rows, func(r *sql.Rows) (MetricsByKey, error) {
		var bk MetricsByKey
		return bk, r.Scan(&bk.KeyID, &bk.KeyName, &bk.Count, &bk.PromptTokens, &bk.OutputTokens, &bk.AvgLatencyMs)
	}); err != nil {
		return nil, err
	}

	rows, err = s.db.Query(`SELECT
		strftime(` + bucketFmt + `, ts/1000, 'unixepoch', 'localtime'),
		COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(tokens_saved), 0)
		FROM events WHERE ts >= ? GROUP BY 1 ORDER BY 1`, since)
	if err != nil {
		return nil, err
	}
	if m.ByDay, err = scanRows(rows, func(r *sql.Rows) (MetricsByDay, error) {
		var bd MetricsByDay
		return bd, r.Scan(&bd.Bucket, &bd.Count, &bd.PromptTokens, &bd.OutputTokens, &bd.TokensSaved)
	}); err != nil {
		return nil, err
	}

	// Compression totals — single row, single round-trip.
	if err := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN compressed = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(tokens_saved), 0),
		COALESCE(SUM(CASE WHEN compressed = 1 THEN prompt_tokens ELSE 0 END), 0),
		COALESCE(SUM(pre_bytes), 0),
		COALESCE(SUM(post_bytes), 0)
		FROM events WHERE ts >= ?`, since).Scan(
		&m.Compression.CompressedRequests, &m.Compression.TokensSaved,
		&m.Compression.PromptTokens,
		&m.Compression.PreBytes, &m.Compression.PostBytes,
	); err != nil {
		return nil, err
	}

	return m, nil
}