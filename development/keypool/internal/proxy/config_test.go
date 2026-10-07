package proxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestDuration_JSONRoundTrip(t *testing.T) {
	cases := []time.Duration{
		60 * time.Second,
		2 * time.Minute,
		0,
		120 * time.Second,
	}
	for _, d := range cases {
		b, err := json.Marshal(Duration(d))
		if err != nil {
			t.Fatalf("marshal %v: %v", d, err)
		}
		// JSON should be a bare integer, not a string.
		if len(b) > 0 && b[0] == '"' {
			t.Fatalf("expected int JSON, got string %s", b)
		}
		var got Duration
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("unmarshal %v: %v", d, err)
		}
		if time.Duration(got) != d {
			t.Fatalf("round-trip %v: got %v", d, time.Duration(got))
		}
	}
}

func TestNewServer_SeedsConfigFromEnv(t *testing.T) {
	s := mustStore(t)
	cfg := Config{
		Upstream:  "https://example.com",
		Cooldown:  Duration(42 * time.Second),
		Port:      "9999",
		MasterKey: mustMasterKey(t),
	}
	srv, err := NewServer(cfg, s)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown(bgCtx())

	// Store should now have a "config" key with the env values.
	val, err := s.GetSetting("config")
	if err != nil {
		t.Fatalf("get setting: %v", err)
	}
	if val == "" {
		t.Fatalf("config not seeded")
	}
	var sc Config
	if err := json.Unmarshal([]byte(val), &sc); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if sc.Upstream != "https://example.com" {
		t.Fatalf("upstream: got %q", sc.Upstream)
	}
	if time.Duration(sc.Cooldown) != 42*time.Second {
		t.Fatalf("cooldown: got %v", time.Duration(sc.Cooldown))
	}
	if sc.Port != "9999" {
		t.Fatalf("port: got %q", sc.Port)
	}
}

func TestNewServer_StoreOverridesEnv(t *testing.T) {
	s := mustStore(t)

	// Pre-seed the store with different values.
	stored := Config{
		Upstream: "https://stored.example.com",
		Cooldown: Duration(99 * time.Second),
		Port:     "8888",
	}
	b, _ := json.Marshal(stored)
	if err := s.SetSetting("config", string(b)); err != nil {
		t.Fatalf("set setting: %v", err)
	}

	// Env values should be overridden by the store.
	cfg := Config{
		Upstream:  "https://env.example.com",
		Cooldown:  Duration(10 * time.Second),
		Port:      "7777",
		MasterKey: mustMasterKey(t),
	}
	srv, err := NewServer(cfg, s)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown(bgCtx())

	snap := srv.configSnapshot()
	if snap.Upstream != "https://stored.example.com" {
		t.Fatalf("upstream: got %q, want stored", snap.Upstream)
	}
	if time.Duration(snap.Cooldown) != 99*time.Second {
		t.Fatalf("cooldown: got %v, want 99s", time.Duration(snap.Cooldown))
	}
	if snap.Port != "8888" {
		t.Fatalf("port: got %q, want 8888", snap.Port)
	}
}

func TestReloadConfig(t *testing.T) {
	s := mustStore(t)
	cfg := Config{
		Upstream:  "https://before.example.com",
		Cooldown:  Duration(30 * time.Second),
		Port:      "1111",
		MasterKey: mustMasterKey(t),
	}
	srv, err := NewServer(cfg, s)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown(bgCtx())

	// Change the store and reload.
	updated := Config{
		Upstream: "https://after.example.com",
		Cooldown: Duration(120 * time.Second),
		Port:     "2222",
	}
	b, _ := json.Marshal(updated)
	if err := s.SetSetting("config", string(b)); err != nil {
		t.Fatalf("set setting: %v", err)
	}
	srv.ReloadConfig()

	snap := srv.configSnapshot()
	if snap.Upstream != "https://after.example.com" {
		t.Fatalf("upstream: got %q", snap.Upstream)
	}
	if time.Duration(snap.Cooldown) != 120*time.Second {
		t.Fatalf("cooldown: got %v", time.Duration(snap.Cooldown))
	}
	if snap.Port != "2222" {
		t.Fatalf("port: got %q", snap.Port)
	}

	// Upstream URL should be re-parsed.
	srv.cfgMu.RLock()
	u := srv.upstreamURL
	srv.cfgMu.RUnlock()
	if u == nil || u.Host != "after.example.com" {
		t.Fatalf("upstream URL: got %v", u)
	}
}

func TestReloadConfig_NilStore(t *testing.T) {
	// ReloadConfig on a nil store should be a no-op, not a panic.
	cfg := Config{
		Upstream:  "https://example.com",
		Cooldown:  Duration(60 * time.Second),
		Port:      "11435",
		MasterKey: mustMasterKey(t),
	}
	srv, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	defer srv.Shutdown(bgCtx())
	srv.ReloadConfig() // must not panic
}

// mustMasterKey generates a valid 32-byte master key for tests.
func mustMasterKey(t *testing.T) []byte {
	t.Helper()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	mk, err := DecodeMasterKey(key)
	if err != nil {
		t.Fatalf("decode master key: %v", err)
	}
	return mk
}

// bgCtx returns a background context — for Shutdown in tests.
func bgCtx() context.Context {
	return context.Background()
}