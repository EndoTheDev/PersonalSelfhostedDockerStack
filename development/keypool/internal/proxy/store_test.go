package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mustStore opens a temp SQLite store with a freshly generated master key.
// t.TempDir() ensures the DB file is cleaned up automatically.
func mustStore(t *testing.T) *Store {
	t.Helper()
	key, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	mk, err := DecodeMasterKey(key)
	if err != nil {
		t.Fatalf("decode master key: %v", err)
	}
	s, err := OpenStore(t.TempDir()+"/test.db", mk)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSeedIfEmpty_SeedThenIdempotent(t *testing.T) {
	s := mustStore(t)

	// First seed: 2 keys into empty store.
	n, err := s.SeedIfEmpty("ollama", "https://ollama.com", []string{"key-a", "key-b"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 keys seeded, got %d", n)
	}

	pid, err := s.FirstProviderID()
	if err != nil {
		t.Fatalf("first provider: %v", err)
	}
	keys, err := s.ListKeys(pid)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys in store, got %d", len(keys))
	}

	// Second seed: no-op (providers already exist).
	n2, err := s.SeedIfEmpty("ollama", "https://ollama.com", []string{"key-c"})
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("expected 0 on second seed, got %d", n2)
	}
	keys2, _ := s.ListKeys(pid)
	if len(keys2) != 2 {
		t.Fatalf("expected still 2 keys after reseed, got %d", len(keys2))
	}
}

func TestSeedIfEmpty_EmptyKeys(t *testing.T) {
	s := mustStore(t)
	n, err := s.SeedIfEmpty("ollama", "https://ollama.com", []string{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 keys for empty input, got %d", n)
	}
	// No provider should have been created.
	pid, _ := s.FirstProviderID()
	if pid != 0 {
		t.Fatalf("expected no provider, got id=%d", pid)
	}
}

func TestAddKey_EmptyName(t *testing.T) {
	s := mustStore(t)
	pid, err := s.AddProvider("ollama", "https://ollama.com")
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	if _, err := s.AddKey(pid, "sk-test", ""); !errors.Is(err, ErrKeyNameEmpty) {
		t.Fatalf("expected ErrKeyNameEmpty for empty name, got %v", err)
	}
	if _, err := s.AddKey(pid, "sk-test", "   "); !errors.Is(err, ErrKeyNameEmpty) {
		t.Fatalf("expected ErrKeyNameEmpty for whitespace name, got %v", err)
	}
}

func TestAddKey_DuplicateName(t *testing.T) {
	s := mustStore(t)
	pid, err := s.AddProvider("ollama", "https://ollama.com")
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	if _, err := s.AddKey(pid, "sk-a", "alpha"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := s.AddKey(pid, "sk-b", "alpha"); !errors.Is(err, ErrKeyNameTaken) {
		t.Fatalf("expected ErrKeyNameTaken for duplicate name, got %v", err)
	}
}

func TestRenameKey_EmptyName(t *testing.T) {
	s := mustStore(t)
	pid, err := s.AddProvider("ollama", "https://ollama.com")
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	id, err := s.AddKey(pid, "sk-test", "alpha")
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	if err := s.RenameKey(id, ""); !errors.Is(err, ErrKeyNameEmpty) {
		t.Fatalf("expected ErrKeyNameEmpty, got %v", err)
	}
}

func TestRenameKey_DuplicateName(t *testing.T) {
	s := mustStore(t)
	pid, err := s.AddProvider("ollama", "https://ollama.com")
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	if _, err := s.AddKey(pid, "sk-a", "alpha"); err != nil {
		t.Fatalf("add alpha: %v", err)
	}
	id2, err := s.AddKey(pid, "sk-b", "beta")
	if err != nil {
		t.Fatalf("add beta: %v", err)
	}
	if err := s.RenameKey(id2, "alpha"); !errors.Is(err, ErrKeyNameTaken) {
		t.Fatalf("expected ErrKeyNameTaken, got %v", err)
	}
}

func TestValidateName_ClampsLongName(t *testing.T) {
	long := strings.Repeat("x", MaxKeyNameLen+10)
	name, err := validateName(long)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(name) != MaxKeyNameLen {
		t.Fatalf("expected name clamped to %d, got %d", MaxKeyNameLen, len(name))
	}
}

func TestWriteKeyNameErr_EmptyMaps400(t *testing.T) {
	w := httptest.NewRecorder()
	writeKeyNameErr(w, ErrKeyNameEmpty)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "name required") {
		t.Fatalf("expected 'name required' in body, got %q", w.Body.String())
	}
}

func TestWriteKeyNameErr_TakenMaps409(t *testing.T) {
	w := httptest.NewRecorder()
	writeKeyNameErr(w, ErrKeyNameTaken)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "already in use") {
		t.Fatalf("expected 'already in use' in body, got %q", w.Body.String())
	}
}

func TestWriteKeyNameErr_UnknownMaps500(t *testing.T) {
	w := httptest.NewRecorder()
	writeKeyNameErr(w, errors.New("boom"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("expected error text in body, got %q", w.Body.String())
	}
}

func TestGetKey_ReturnsDecryptedPlaintext(t *testing.T) {
	s := mustStore(t)
	pid, err := s.AddProvider("ollama", "https://ollama.com")
	if err != nil {
		t.Fatalf("add provider: %v", err)
	}
	id, err := s.AddKey(pid, "sk-secret", "alpha")
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	k, err := s.GetKey(id)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if k.Plaintext != "sk-secret" {
		t.Fatalf("expected plaintext 'sk-secret', got %q", k.Plaintext)
	}
	if k.Name != "alpha" {
		t.Fatalf("expected name 'alpha', got %q", k.Name)
	}
}

func TestGetKey_NotFound(t *testing.T) {
	s := mustStore(t)
	_, err := s.GetKey(999)
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

func TestUpdateKeyState_LastUsedRoundTrip(t *testing.T) {
	s := mustStore(t)
	pid, _ := s.AddProvider("ollama", "https://ollama.com")
	id, _ := s.AddKey(pid, "sk-test", "alpha")

	// Fresh key: last_used should be nil (0 in DB).
	keys, _ := s.ListKeys(pid)
	if keys[0].LastUsed != nil {
		t.Fatalf("expected nil LastUsed on fresh key, got %v", keys[0].LastUsed)
	}

	// Update with a timestamp (Unix-second precision — DB stores int seconds).
	ts := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	if err := s.UpdateKeyState(id, 1, nil, ts); err != nil {
		t.Fatalf("update key state: %v", err)
	}

	// Reload and verify.
	keys2, _ := s.ListKeys(pid)
	if keys2[0].LastUsed == nil {
		t.Fatal("expected non-nil LastUsed after update")
	}
	if !keys2[0].LastUsed.Equal(ts) {
		t.Fatalf("expected LastUsed %v, got %v", ts, keys2[0].LastUsed)
	}
}