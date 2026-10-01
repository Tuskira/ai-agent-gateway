// Package apikey implements gateway-issued API key credentials: plaintext
// generation, hashing for storage/lookup, and an Authenticator (see
// authenticator.go) that resolves a request's key to a Principal.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"strings"
)

// Prefix is prepended to every plaintext key the gateway issues, and is
// what distinguishes a gateway API key from any other bearer credential
// (e.g. a JWT) on the wire.
const Prefix = "gk_"

// PrefixDisplayLen is the length of the plaintext slice (Prefix included)
// kept as APIKey.KeyPrefix for display purposes (e.g. an admin UI's key
// list): "gk_" plus 8 characters of the random suffix.
const PrefixDisplayLen = len(Prefix) + 8

// randomBytes is how much entropy Generate draws for the plaintext suffix.
const randomBytes = 32

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Generate mints a new API key.
//
//   - plaintext is handed to the caller exactly once and never stored;
//     it is Prefix followed by the base62 encoding of 32 crypto/rand
//     bytes.
//   - hash is Hash(plaintext), the value actually persisted and looked
//     up on every request.
//   - prefix is plaintext's first PrefixDisplayLen characters, safe to
//     store and display alongside hash (e.g. "gk_A1b2C3d4") since it
//     alone isn't enough entropy to reconstruct the key.
func Generate() (plaintext, hash, prefix string, err error) {
	buf := make([]byte, randomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", "", fmt.Errorf("apikey: generate random bytes: %w", err)
	}

	plaintext = Prefix + base62Encode(buf)
	hash = Hash(plaintext)

	prefix = plaintext
	if len(prefix) > PrefixDisplayLen {
		prefix = plaintext[:PrefixDisplayLen]
	}

	return plaintext, hash, prefix, nil
}

// Hash returns the hex-encoded SHA-256 digest of plaintext: the form
// persisted in the store and used to look up a key on every request. The
// plaintext itself is never stored.
func Hash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ParseBearer extracts a gateway API key from r: either an
// "X-Gateway-Key: gk_..." header or an "Authorization: Bearer gk_..."
// header (checked in that order). X-Gateway-Key is the dedicated
// gateway-credential channel and takes precedence, so a BYOK Authorization
// header carrying an upstream provider's own key (e.g. "Bearer sk-..." for
// OpenAI, or "AWS4-HMAC-SHA256 ..." for Bedrock) does NOT shadow the gateway
// key. ok is false when neither header carries a gateway credential, or when
// the credential present doesn't start with Prefix -- e.g. a provider key or
// a JWT in the Authorization header -- so the caller isn't ours and an auth
// chain should try the next Authenticator rather than treating this as an
// invalid gateway key.
func ParseBearer(r *http.Request) (string, bool) {
	// X-Gateway-Key takes precedence, but only when it actually carries a
	// gk_ key: a present-but-non-gk_ value (junk, or a stray header) must fall
	// through to Authorization rather than short-circuit to a 401.
	if v := r.Header.Get("X-Gateway-Key"); v != "" {
		if cred, ok := checkPrefix(v); ok {
			return cred, true
		}
	}

	if auth := r.Header.Get("Authorization"); auth != "" {
		const bearerPrefix = "Bearer "
		if strings.HasPrefix(auth, bearerPrefix) {
			return checkPrefix(strings.TrimPrefix(auth, bearerPrefix))
		}
	}

	return "", false
}

func checkPrefix(cred string) (string, bool) {
	if !strings.HasPrefix(cred, Prefix) {
		return "", false
	}
	return cred, true
}

// base62Encode returns b's big-endian value rendered in base62 digits
// (base62Alphabet). It has no fixed output width -- Generate doesn't need
// one, since a key's contract is its gk_ prefix and its entropy, not a
// specific length.
func base62Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	if n.Sign() == 0 {
		return string(base62Alphabet[0])
	}

	base := big.NewInt(62)
	mod := new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, base62Alphabet[mod.Int64()])
	}
	// out was built least-significant digit first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}
