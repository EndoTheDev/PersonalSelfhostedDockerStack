// Command keypool is the container-native rewrite of allocate.d: a reverse
// proxy in front of Ollama Cloud that rotates N API keys LRU (or least_used),
// benches them on 429/5xx-ex-502, and retries once.
//
// Container differences from allocate.d:
//   - No CLI dispatch, no tray, no FindEnvUp .env walk. Config is env-only:
//     KEYPOOL_UPSTREAM, KEYPOOL_PORT, KEYPOOL_COOLDOWN, KEYPOOL_TIMEOUT.
//   - The master key (KEYPOOL_MASTER_KEY) is required in the environment —
//     the container is a fresh world each boot; writing .env back is the
//     compose file's job, not the daemon's.
//   - ALLOCATED_KEYS (comma-separated) is a one-shot seed on first run; the
//     SQLite store is the source of truth afterwards.
package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	_ "modernc.org/sqlite"

	"keypool/internal/proxy"
	"keypool/internal/web"
)

const defaultDBPath = "/data/keypool.db"

func main() {
	dbPath := envOr("KEYPOOL_DB", defaultDBPath)
	if err := ensureMasterKey(dbPath); err != nil {
		log.Fatalf("master key: %v", err)
	}
	bridgeEnv()
	cfg, err := proxy.LoadConfigFromEnv("")
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := proxy.OpenStore(dbPath, cfg.MasterKey)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if migrated, err := seedFromEnv(store, cfg); err != nil {
		log.Fatalf("seed: %v", err)
	} else if migrated > 0 {
		log.Printf("migrated %d keys from environment (KEYPOOL_KEYS can now be removed)", migrated)
	}

	srv, err := proxy.NewServer(cfg, store)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	srv.UIHandler = web.Handler()
	if err := srv.ListenAndServe(envBool("KEYPOOL_DEBUG", false)); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
}

// ensureMasterKey guards against the allocate.d footgun: a missing master key
// with an existing database of encrypted keys is unrecoverable. In a
// container the key is env-injected by compose; refuse to regenerate rather
// than silently orphaning the store.
func ensureMasterKey(dbPath string) error {
	if os.Getenv("KEYPOOL_MASTER_KEY") != "" {
		return nil
	}
	if dbHasKey(dbPath) {
		return errors.New("KEYPOOL_MASTER_KEY missing but the store has encrypted keys — restore the env var to decrypt them")
	}
	// Fresh deployment with no key: generate one and log it ONCE so the
	// operator can pin it in compose before the next restart. ponytail:
	// ceiling — printing a secret to logs is acceptable exactly once, on
	// first boot, when the alternative is an unusable service.
	key, err := proxy.GenerateMasterKey()
	if err != nil {
		return err
	}
	os.Setenv("KEYPOOL_MASTER_KEY", key)
	log.Printf("KEYPOOL_MASTER_KEY not set; generated a fresh key — pin it in the compose .env before restarting: %s", key)
	return nil
}

// bridgeEnv copies KEYPOOL_* env vars to the ALLOCATED_* names the carried
// proxy package reads. ponytail: the proxy's env handling is proven and
// test-covered under the old names; bridging here is fewer edited lines (and
// fewer re-test surfaces) than renaming across the package. If keypool ever
// grows beyond the alias, rename inside the package instead.
func bridgeEnv() {
	aliases := [][2]string{
		{"KEYPOOL_MASTER_KEY", "ALLOCATED_MASTER_KEY"},
		{"KEYPOOL_UPSTREAM", "ALLOCATED_UPSTREAM"},
		{"KEYPOOL_COOLDOWN", "ALLOCATED_COOLDOWN"},
		{"KEYPOOL_TIMEOUT", "ALLOCATED_TIMEOUT"},
		{"KEYPOOL_PORT", "ALLOCATED_PORT"},
	}
	for _, a := range aliases {
		if v := strings.TrimSpace(os.Getenv(a[0])); v != "" {
			os.Setenv(a[1], v)
		}
	}
}

// dbHasKey returns true if the SQLite database at path exists and has at
// least one row in the keys table. Row count is plaintext metadata; no
// decryption happens here.
func dbHasKey(dbPath string) bool {
	if _, err := os.Stat(dbPath); err != nil {
		return false
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM keys").Scan(&n); err != nil {
		return false // no keys table → fresh DB → treat as no keys
	}
	return n > 0
}

// seedFromEnv migrates KEYPOOL_KEYS (comma-separated) into the store on first
// run. One-shot: skipped once the store has any provider.
func seedFromEnv(store *proxy.Store, cfg proxy.Config) (int, error) {
	raw := strings.TrimSpace(os.Getenv("KEYPOOL_KEYS"))
	if raw == "" {
		return 0, nil
	}
	var keys []string
	for _, k := range strings.Split(raw, ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	return store.SeedIfEmpty("ollama", cfg.Upstream, keys)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	return def
}