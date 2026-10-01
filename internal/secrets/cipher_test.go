package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
)

func testKeyRing(t *testing.T) *KeyRing {
	t.Helper()
	return &KeyRing{
		Keys:        map[string][]byte{"k1": bytes32('a'), "k2": bytes32('b')},
		ActiveKeyID: "k1",
	}
}

func bytes32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	ring := testKeyRing(t)
	plaintext := []byte(`{"client_secret":"s3cr3t"}`)

	ciphertext, nonce, err := ring.Encrypt("k1", plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(nonce) != 12 {
		t.Errorf("nonce length = %d, want 12", len(nonce))
	}
	if string(ciphertext) == string(plaintext) {
		t.Error("ciphertext must not equal plaintext")
	}

	got, err := ring.Decrypt("k1", ciphertext, nonce)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("Decrypt = %q, want %q", got, plaintext)
	}
}

func TestEncrypt_NoncesAreRandom(t *testing.T) {
	ring := testKeyRing(t)
	_, nonce1, err := ring.Encrypt("k1", []byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, nonce2, err := ring.Encrypt("k1", []byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if string(nonce1) == string(nonce2) {
		t.Error("two Encrypt calls produced the same nonce")
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	ring := testKeyRing(t)
	ciphertext, nonce, err := ring.Encrypt("k1", []byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if _, err := ring.Decrypt("k2", ciphertext, nonce); err == nil {
		t.Error("Decrypt with wrong key id succeeded, want error")
	}
}

func TestDecrypt_TamperedCiphertextFails(t *testing.T) {
	ring := testKeyRing(t)
	ciphertext, nonce, err := ring.Encrypt("k1", []byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	tampered := append([]byte(nil), ciphertext...)
	tampered[0] ^= 0xFF

	if _, err := ring.Decrypt("k1", tampered, nonce); err == nil {
		t.Error("Decrypt of tampered ciphertext succeeded, want error")
	}
}

func TestDecrypt_TamperedNonceFails(t *testing.T) {
	ring := testKeyRing(t)
	ciphertext, nonce, err := ring.Encrypt("k1", []byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	tampered := append([]byte(nil), nonce...)
	tampered[0] ^= 0xFF

	if _, err := ring.Decrypt("k1", ciphertext, tampered); err == nil {
		t.Error("Decrypt with tampered nonce succeeded, want error")
	}
}

func TestEncrypt_UnknownKeyID(t *testing.T) {
	ring := testKeyRing(t)
	if _, _, err := ring.Encrypt("no-such-key", []byte("x")); err == nil {
		t.Error("Encrypt with unknown key id succeeded, want error")
	}
}

func TestDecrypt_UnknownKeyID(t *testing.T) {
	ring := testKeyRing(t)
	if _, err := ring.Decrypt("no-such-key", []byte("x"), bytes32('n')[:12]); err == nil {
		t.Error("Decrypt with unknown key id succeeded, want error")
	}
}

func TestAAD_BoundToKeyID(t *testing.T) {
	// A ciphertext/nonce pair sealed under key "k1" must not decrypt under
	// "k2" even when "k2" happens to hold the identical key bytes -- AAD
	// is the key ID string itself, not the key material.
	ring := &KeyRing{
		Keys:        map[string][]byte{"k1": bytes32('a'), "k2": bytes32('a')},
		ActiveKeyID: "k1",
	}
	ciphertext, nonce, err := ring.Encrypt("k1", []byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := ring.Decrypt("k2", ciphertext, nonce); err == nil {
		t.Error("Decrypt under a different key id (same key bytes) succeeded, want error (AAD mismatch)")
	}
}

func TestGenerateKey(t *testing.T) {
	k1 := GenerateKey()
	k2 := GenerateKey()
	if k1 == k2 {
		t.Error("GenerateKey produced identical output twice")
	}

	decoded, err := base64.StdEncoding.DecodeString(k1)
	if err != nil {
		t.Fatalf("GenerateKey output is not valid base64: %v", err)
	}
	if len(decoded) != keySize {
		t.Errorf("decoded key length = %d, want %d", len(decoded), keySize)
	}
}

func TestFingerprint(t *testing.T) {
	key := bytes32('a')
	fp1 := Fingerprint(key)
	fp2 := Fingerprint(key)
	if fp1 != fp2 {
		t.Errorf("Fingerprint not deterministic: %q vs %q", fp1, fp2)
	}
	if len(fp1) != 8 {
		t.Errorf("Fingerprint length = %d, want 8", len(fp1))
	}
	if strings.Contains(fp1, string(key)) {
		t.Error("Fingerprint must not contain the raw key")
	}

	other := Fingerprint(bytes32('b'))
	if fp1 == other {
		t.Error("Fingerprint collided for different keys")
	}
}

// ---------------------------------------------------------------------------
// LoadKeyRing
// ---------------------------------------------------------------------------

func TestLoadKeyRing_SingleKeyFromEnv(t *testing.T) {
	key := bytes32('a')
	t.Setenv("GATEWAY_MASTER_KEY", base64.StdEncoding.EncodeToString(key))

	ring, err := LoadKeyRing(config.SecretStore{})
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	if ring.ActiveKeyID != "k1" {
		t.Errorf("ActiveKeyID = %q, want default k1", ring.ActiveKeyID)
	}
	if string(ring.Keys["k1"]) != string(key) {
		t.Error("ring key material does not match the env var")
	}
}

func TestLoadKeyRing_SingleKeyHexEncoded(t *testing.T) {
	key := bytes32('c')
	t.Setenv("GATEWAY_MASTER_KEY", hex.EncodeToString(key))

	ring, err := LoadKeyRing(config.SecretStore{})
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	if string(ring.Keys["k1"]) != string(key) {
		t.Error("ring key material does not match the hex-encoded env var")
	}
}

func TestLoadKeyRing_MultiKeyList(t *testing.T) {
	k1 := bytes32('a')
	k2 := bytes32('b')
	raw := "k1:" + base64.StdEncoding.EncodeToString(k1) + ",k2:" + base64.StdEncoding.EncodeToString(k2)
	t.Setenv("GATEWAY_MASTER_KEY", raw)

	ring, err := LoadKeyRing(config.SecretStore{ActiveKeyID: "k2"})
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	if ring.ActiveKeyID != "k2" {
		t.Errorf("ActiveKeyID = %q, want k2", ring.ActiveKeyID)
	}
	if len(ring.Keys) != 2 {
		t.Fatalf("ring has %d keys, want 2", len(ring.Keys))
	}
	if string(ring.Keys["k1"]) != string(k1) || string(ring.Keys["k2"]) != string(k2) {
		t.Error("ring key material does not match")
	}
}

func TestLoadKeyRing_ActiveKeyIDMustExist(t *testing.T) {
	t.Setenv("GATEWAY_MASTER_KEY", "k1:"+base64.StdEncoding.EncodeToString(bytes32('a')))

	if _, err := LoadKeyRing(config.SecretStore{ActiveKeyID: "k9"}); err == nil {
		t.Error("LoadKeyRing with a missing active_key_id succeeded, want error")
	}
}

func TestLoadKeyRing_Unset(t *testing.T) {
	t.Setenv("GATEWAY_MASTER_KEY", "")
	// Also ensure no stray master-key file is configured.
	if _, err := LoadKeyRing(config.SecretStore{}); err == nil {
		t.Error("LoadKeyRing with no key configured succeeded, want error")
	}
}

func TestLoadKeyRing_CustomEnvName(t *testing.T) {
	key := bytes32('a')
	t.Setenv("MY_CUSTOM_KEY", base64.StdEncoding.EncodeToString(key))

	ring, err := LoadKeyRing(config.SecretStore{MasterKeyEnv: "MY_CUSTOM_KEY"})
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	if string(ring.Keys["k1"]) != string(key) {
		t.Error("ring key material does not match the custom env var")
	}
}

func TestLoadKeyRing_FromFile(t *testing.T) {
	key := bytes32('a')
	path := t.TempDir() + "/master.key"
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	ring, err := LoadKeyRing(config.SecretStore{MasterKeyFile: path})
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	if string(ring.Keys["k1"]) != string(key) {
		t.Error("ring key material does not match the key file")
	}
}

func TestLoadKeyRing_EnvWinsOverFile(t *testing.T) {
	envKey := bytes32('a')
	fileKey := bytes32('b')
	path := t.TempDir() + "/master.key"
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(fileKey)), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	t.Setenv("GATEWAY_MASTER_KEY", base64.StdEncoding.EncodeToString(envKey))

	ring, err := LoadKeyRing(config.SecretStore{MasterKeyFile: path})
	if err != nil {
		t.Fatalf("LoadKeyRing: %v", err)
	}
	if string(ring.Keys["k1"]) != string(envKey) {
		t.Error("env var should win over master_key_file")
	}
}

func TestLoadKeyRing_InvalidKeyMaterial(t *testing.T) {
	t.Setenv("GATEWAY_MASTER_KEY", "not-a-valid-key")
	if _, err := LoadKeyRing(config.SecretStore{}); err == nil {
		t.Error("LoadKeyRing with invalid key material succeeded, want error")
	}
}
