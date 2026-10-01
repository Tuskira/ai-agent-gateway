// Package secrets implements the gateway's encrypted secret store: a
// master-key-backed AES-256-GCM cipher (this file), a Service that layers
// encrypt/decrypt/rotate/cache semantics on top of pkg/store.CredentialStore
// (store.go), and the "secret_store"/"env"/"file" header providers that let
// a connector's outbound headers pull from it (providers.go).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/Tuskira/tusk-ai-secured-gateway/internal/config"
)

const (
	// keySize is the AES-256 key length in bytes.
	keySize = 32

	// defaultMasterKeyEnv is used when config.SecretStore.MasterKeyEnv is
	// left empty.
	defaultMasterKeyEnv = "GATEWAY_MASTER_KEY"
	// defaultActiveKeyID is used when config.SecretStore.ActiveKeyID is
	// left empty.
	defaultActiveKeyID = "k1"
)

// KeyRing holds the gateway's master key material: every key currently
// known (so rows encrypted under a retired key can still be decrypted, and
// re-encrypted onto the active one -- see Service.ReEncryptAll), and which
// one new Encrypt calls use.
type KeyRing struct {
	// Keys maps a key id (e.g. "k1") to its 32-byte AES-256 key.
	Keys map[string][]byte
	// ActiveKeyID is the key new Encrypt calls use. It must be present in
	// Keys.
	ActiveKeyID string
}

// LoadKeyRing builds a KeyRing from cfg. The key material is read from the
// environment variable named by cfg.MasterKeyEnv (default
// GATEWAY_MASTER_KEY) if set, else from the file at cfg.MasterKeyFile if
// set; it is an error for neither to be configured (or for the configured
// source to be empty). Generate a value with `openssl rand -base64 32` or
// `gateway secrets genkey`.
//
// The value is either:
//   - a single base64- or hex-encoded 32-byte key, assigned to
//     cfg.ActiveKeyID (default "k1"); or
//   - a comma-separated "id:key" list, e.g. "k1:<b64>,k2:<b64>", to allow
//     rotation (see Service.ReEncryptAll and `gateway secrets rekey`).
//
// cfg.ActiveKeyID must name a key present in the resulting ring.
func LoadKeyRing(cfg config.SecretStore) (*KeyRing, error) {
	envName := cfg.MasterKeyEnv
	if envName == "" {
		envName = defaultMasterKeyEnv
	}
	activeID := cfg.ActiveKeyID
	if activeID == "" {
		activeID = defaultActiveKeyID
	}

	raw, err := readKeyMaterial(envName, cfg.MasterKeyFile)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, fmt.Errorf(
			"secrets: no master key configured -- set %s, or secret_store.master_key_file, to a key generated with `openssl rand -base64 32` (or `gateway secrets genkey`)",
			envName,
		)
	}

	ring, err := parseKeyRing(raw, activeID)
	if err != nil {
		return nil, err
	}
	if _, ok := ring.Keys[ring.ActiveKeyID]; !ok {
		return nil, fmt.Errorf("secrets: active_key_id %q is not present in the configured key ring", ring.ActiveKeyID)
	}
	return ring, nil
}

// readKeyMaterial returns the raw (untrimmed-source, but whitespace-
// trimmed-on-read) key material: the environment variable envName if set,
// else the contents of filePath if non-empty, else "" (neither source
// configured -- the caller decides whether that's an error).
func readKeyMaterial(envName, filePath string) (string, error) {
	if v := os.Getenv(envName); v != "" {
		return strings.TrimSpace(v), nil
	}
	if filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("secrets: read master_key_file %q: %w", filePath, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return "", nil
}

// parseKeyRing parses raw per the format documented on LoadKeyRing. The
// ":" discriminator between the two formats is safe: neither base64 nor
// hex alphabets contain ':'.
func parseKeyRing(raw, activeID string) (*KeyRing, error) {
	ring := &KeyRing{Keys: map[string][]byte{}, ActiveKeyID: activeID}

	if !strings.Contains(raw, ":") {
		key, err := decodeKey(raw)
		if err != nil {
			return nil, fmt.Errorf("secrets: master key: %w", err)
		}
		ring.Keys[activeID] = key
		return ring, nil
	}

	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("secrets: malformed key ring entry %q, want \"id:key\"", entry)
		}
		id := strings.TrimSpace(parts[0])
		if id == "" {
			return nil, fmt.Errorf("secrets: malformed key ring entry %q, empty key id", entry)
		}
		key, err := decodeKey(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("secrets: key %q: %w", id, err)
		}
		ring.Keys[id] = key
	}
	if len(ring.Keys) == 0 {
		return nil, fmt.Errorf("secrets: key ring is empty")
	}
	return ring, nil
}

// decodeKey decodes s as either standard base64 or hex, accepting whichever
// encoding yields exactly keySize bytes.
func decodeKey(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == keySize {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil && len(b) == keySize {
		return b, nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == keySize {
		return b, nil
	}
	return nil, fmt.Errorf("not a valid %d-byte key (base64 or hex encoded)", keySize)
}

// GenerateKey returns a fresh, random, base64-encoded 32-byte AES-256 key,
// suitable for GATEWAY_MASTER_KEY. Used by `gateway secrets genkey`.
func GenerateKey() string {
	b := make([]byte, keySize)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read only fails if the OS CSPRNG itself is
		// unavailable, which is unrecoverable; a generated key that
		// silently used weak/short randomness would be far worse than
		// crashing here.
		panic("secrets: crypto/rand.Read failed: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(b)
}

// Fingerprint returns a short, non-reversible identifier for key (the
// first 8 hex characters of its SHA-256 digest). It is safe to log --
// e.g. at startup, or after a rekey -- to confirm which key material is
// active across environments without ever revealing the key itself.
func Fingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:8]
}

// Encrypt seals plaintext under the ring's key keyID with AES-256-GCM,
// using a fresh random 12-byte nonce and the key id itself as additional
// authenticated data (AAD) -- so a ciphertext produced for one key id can
// never be decrypted (even if the bytes were copied) under a different one.
func (k *KeyRing) Encrypt(keyID string, plaintext []byte) (ciphertext, nonce []byte, err error) {
	gcm, err := k.gcm(keyID)
	if err != nil {
		return nil, nil, err
	}

	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secrets: generate nonce: %w", err)
	}

	ciphertext = gcm.Seal(nil, nonce, plaintext, []byte(keyID))
	return ciphertext, nonce, nil
}

// Decrypt opens ciphertext/nonce, previously produced by Encrypt(keyID,
// ...). It fails if keyID is unknown, if nonce/ciphertext were tampered
// with, or if either was produced under a different key id (the AAD check).
func (k *KeyRing) Decrypt(keyID string, ciphertext, nonce []byte) ([]byte, error) {
	gcm, err := k.gcm(keyID)
	if err != nil {
		return nil, err
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(keyID))
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt: %w", err)
	}
	return plaintext, nil
}

func (k *KeyRing) gcm(keyID string) (cipher.AEAD, error) {
	key, ok := k.Keys[keyID]
	if !ok {
		return nil, fmt.Errorf("secrets: unknown key id %q", keyID)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: new AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: new GCM: %w", err)
	}
	return gcm, nil
}
