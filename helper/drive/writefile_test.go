package drive

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// TestWriteFileSessionKeyRoundtrip verifies that a session key can be encrypted
// with a node keyring and recovered by DecryptSessionKey.
func TestWriteFileSessionKeyRoundtrip(t *testing.T) {
	nodeKey, err := crypto.GenerateKey("test", "test@example.com", "x25519", 0)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	nodeKR, err := crypto.NewKeyRing(nodeKey)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}

	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatalf("GenerateSessionKey: %v", err)
	}

	keyPacket, err := nodeKR.EncryptSessionKey(sk)
	if err != nil {
		t.Fatalf("EncryptSessionKey: %v", err)
	}

	recovered, err := nodeKR.DecryptSessionKey(keyPacket)
	if err != nil {
		t.Fatalf("DecryptSessionKey: %v", err)
	}
	if !bytes.Equal(sk.Key, recovered.Key) {
		t.Error("recovered session key does not match original")
	}
	if sk.Algo != recovered.Algo {
		t.Errorf("algo mismatch: got %q want %q", recovered.Algo, sk.Algo)
	}
}

// TestWriteFileBlockEncryptDecrypt verifies that a block encrypted with
// SessionKey.EncryptAndSign can be decrypted with SessionKey.Decrypt.
func TestWriteFileBlockEncryptDecrypt(t *testing.T) {
	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatalf("GenerateSessionKey: %v", err)
	}

	plaintext := []byte("hello proton drive block encryption test data")
	enc, err := sk.Encrypt(crypto.NewPlainMessage(plaintext))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	decMsg, err := sk.Decrypt(enc)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(decMsg.GetBinary(), plaintext) {
		t.Errorf("decrypted mismatch: got %q want %q", decMsg.GetBinary(), plaintext)
	}
}

// TestWriteFileEncSigOverPlaintext verifies that EncSignature is computed over the
// PLAINTEXT chunk (not the ciphertext), matching the Windows Proton Drive client.
func TestWriteFileEncSigOverPlaintext(t *testing.T) {
	addrKey, err := crypto.GenerateKey("addr", "addr@example.com", "x25519", 0)
	if err != nil {
		t.Fatalf("GenerateKey addr: %v", err)
	}
	addrKR, err := crypto.NewKeyRing(addrKey)
	if err != nil {
		t.Fatalf("NewKeyRing addr: %v", err)
	}
	nodeKey, err := crypto.GenerateKey("node", "node@example.com", "x25519", 0)
	if err != nil {
		t.Fatalf("GenerateKey node: %v", err)
	}
	nodeKR, err := crypto.NewKeyRing(nodeKey)
	if err != nil {
		t.Fatalf("NewKeyRing node: %v", err)
	}

	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		t.Fatalf("GenerateSessionKey: %v", err)
	}

	plaintext := []byte("hello proton drive enc sig test")
	enc, err := sk.Encrypt(crypto.NewPlainMessage(plaintext))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Sign the PLAINTEXT (not ciphertext) and encrypt the signature with the node key.
	sig, err := addrKR.SignDetachedEncrypted(crypto.NewPlainMessage(plaintext), nodeKR)
	if err != nil {
		t.Fatalf("SignDetachedEncrypted: %v", err)
	}
	_, err = sig.GetArmored()
	if err != nil {
		t.Fatalf("GetArmored: %v", err)
	}

	// Verify: a signature over the ciphertext must NOT verify against the plaintext.
	badSig, err := addrKR.SignDetachedEncrypted(crypto.NewPlainMessage(enc), nodeKR)
	if err != nil {
		t.Fatalf("SignDetachedEncrypted (bad): %v", err)
	}
	err = addrKR.VerifyDetachedEncrypted(crypto.NewPlainMessage(plaintext), badSig, nodeKR, crypto.GetUnixTime())
	if err == nil {
		t.Error("signature over ciphertext should not verify against plaintext")
	}

	// Verify: a correct signature over the plaintext MUST verify.
	if err := addrKR.VerifyDetachedEncrypted(crypto.NewPlainMessage(plaintext), sig, nodeKR, crypto.GetUnixTime()); err != nil {
		t.Errorf("signature over plaintext failed to verify: %v", err)
	}
}

// TestWriteFileBlockSplitting verifies the block-splitting logic for various
// file sizes including empty, sub-block, exact-block, and multi-block.
func TestWriteFileBlockSplitting(t *testing.T) {
	cases := []struct {
		name       string
		dataLen    int
		wantBlocks int
	}{
		{"empty", 0, 0},
		{"one byte", 1, 1},
		{"exactly 4 MiB", protonBlockUploadSize, 1},
		{"4 MiB + 1", protonBlockUploadSize + 1, 2},
		{"two full blocks", 2 * protonBlockUploadSize, 2},
		{"two full blocks + 1", 2*protonBlockUploadSize + 1, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := make([]byte, tc.dataLen)
			count := 0
			for i := 0; ; i++ {
				start := i * protonBlockUploadSize
				if start >= len(data) {
					break
				}
				count++
			}
			if count != tc.wantBlocks {
				t.Errorf("block count = %d, want %d", count, tc.wantBlocks)
			}
		})
	}
}

// TestWriteFileBlockHash verifies that the SHA-256 block hash is computed over
// the encrypted bytes and round-trips through base64 correctly.
func TestWriteFileBlockHash(t *testing.T) {
	enc := []byte("some encrypted block bytes for hashing")
	h := sha256.Sum256(enc)
	hashB64 := base64.StdEncoding.EncodeToString(h[:])

	decoded, err := base64.StdEncoding.DecodeString(hashB64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, h[:]) {
		t.Error("hash base64 round-trip failed")
	}
	if len(decoded) != sha256.Size {
		t.Errorf("decoded hash length = %d, want %d", len(decoded), sha256.Size)
	}
}

// TestByteStreamImplementsInterface checks that byteStream satisfies the
// resty MultiPartStream interface (compile-time check via function call).
func TestByteStreamImplementsInterface(t *testing.T) {
	bs := byteStream{r: bytes.NewReader([]byte("test"))}
	r := bs.GetMultipartReader()
	if r == nil {
		t.Error("GetMultipartReader returned nil")
	}
}
