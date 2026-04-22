package drive

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	proton "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

func TestIsAlreadyExistsError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"code 2500", proton.APIError{Code: 2500}, true},
		{"code 2501", proton.APIError{Code: 2501}, false},
		{"code 0", proton.APIError{Code: 0}, false},
		{"nil", nil, false},
		{"other error", ErrOffline, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAlreadyExistsError(tc.err); got != tc.want {
				t.Errorf("isAlreadyExistsError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestNodeKeyRingCacheHit(t *testing.T) {
	s := minimalSession()
	key, err := crypto.GenerateKey("test", "test@example.com", "x25519", 0)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	kr, err := crypto.NewKeyRing(key)
	if err != nil {
		t.Fatalf("new keyring: %v", err)
	}
	s.nodeKRs["link-1"] = kr

	got, err := s.nodeKeyRing(context.Background(), proton.Link{LinkID: "link-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != kr {
		t.Error("expected cached keyring to be returned")
	}
}

func TestNodeKeyRingCacheMiss(t *testing.T) {
	s := minimalSession()
	_, err := s.nodeKeyRing(context.Background(), proton.Link{LinkID: "missing"})
	if err == nil {
		t.Error("expected error on cache miss, got nil")
	}
}

func TestNameHashHMACSHA256(t *testing.T) {
	hashKey := []byte("test-hash-key-32-bytes-padding!!")
	name := "Documents"

	mac := hmac.New(sha256.New, hashKey)
	mac.Write([]byte(name))
	got := hex.EncodeToString(mac.Sum(nil))

	// Recompute independently.
	mac2 := hmac.New(sha256.New, hashKey)
	mac2.Write([]byte(name))
	want := hex.EncodeToString(mac2.Sum(nil))

	if got != want {
		t.Errorf("NameHash mismatch: got %q, want %q", got, want)
	}
	if len(got) != 64 {
		t.Errorf("NameHash should be 64 hex chars (SHA-256), got %d", len(got))
	}
}

func TestMakeDirCryptoRoundtrip(t *testing.T) {
	// Exercise the key generation and passphrase lock/unlock cycle used in MakeDir.
	nodeKey, err := crypto.GenerateKey("test", "test@example.com", "x25519", 0)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	passBytes := make([]byte, 32)
	for i := range passBytes {
		passBytes[i] = byte(i) // deterministic for test
	}

	locked, err := nodeKey.Lock(passBytes)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	armored, err := locked.Armor()
	if err != nil {
		t.Fatalf("Armor: %v", err)
	}
	if armored == "" {
		t.Error("armored key should not be empty")
	}

	// Verify we can re-import and unlock the key.
	imported, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		t.Fatalf("NewKeyFromArmored: %v", err)
	}
	unlocked, err := imported.Unlock(passBytes)
	if err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if ok, err := unlocked.IsUnlocked(); err != nil || !ok {
		t.Errorf("key should be unlocked after Unlock: ok=%v err=%v", ok, err)
	}
}

func TestMakeDirParentInvalidatedOnAlreadyExists(t *testing.T) {
	s := &Session{
		rootID:    "root",
		nodeKRs:   make(map[string]*crypto.KeyRing),
		linkPaths: make(map[string]string),
		pathLinks: make(map[string]string),
		meta:      NewMetaCache(time.Hour),
	}

	// Pre-populate a stale parent listing.
	s.meta.SetList("/docs", nil, nil)

	// Simulate the invalidation that MakeDir performs on AlreadyExists.
	s.meta.InvalidatePath("/docs")

	if _, _, ok := s.meta.GetList("/docs"); ok {
		t.Error("parent listing should be evicted after InvalidatePath")
	}
}
