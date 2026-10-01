// Package password implements the gateway's console-user password
// handling: argon2id hashing in the standard PHC string format,
// constant-time verification, the password policy, and generation of the
// one-time temporary passwords an admin hands to a user.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Hashing parameters for new hashes: 64 MiB of memory, 3 passes, 2 lanes,
// a 16-byte salt and a 32-byte key. Verify reads the parameters back out
// of the stored PHC string, so these can be raised later without
// invalidating existing hashes.
const (
	memoryKiB  uint32 = 64 * 1024
	iterations uint32 = 3
	lanes      uint8  = 2
	saltLen           = 16
	keyLen            = 32

	// maxMemoryKiB and maxIterations bound what Verify will accept from a
	// stored hash, so a corrupt or hostile row cannot make a login
	// allocate gigabytes or spin for minutes.
	maxMemoryKiB  uint32 = 1 << 20 // 1 GiB
	maxIterations uint32 = 16
	maxLanes      uint8  = 16
)

// Password policy.
const (
	MinLength = 12
	MaxLength = 128
)

// Policy errors returned by ValidatePolicy. Their messages are safe to
// show to the user.
var (
	ErrTooShort       = fmt.Errorf("password must be at least %d characters", MinLength)
	ErrTooLong        = fmt.Errorf("password must be at most %d characters", MaxLength)
	ErrEqualsUsername = errors.New("password must not be the same as the username")
)

// ErrMalformedHash means a stored hash is not a parseable argon2id PHC
// string with acceptable parameters.
var ErrMalformedHash = errors.New("password: malformed argon2id hash")

// hashSlots bounds how many hashes run at once: each holds 64 MiB, and a
// burst of logins must not be able to exhaust the process's memory.
var hashSlots = make(chan struct{}, max(2, runtime.NumCPU()))

func acquire() func() {
	hashSlots <- struct{}{}
	return func() { <-hashSlots }
}

// Hash returns the argon2id PHC string for plain:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
//
// with unpadded standard base64 for salt and hash.
func Hash(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: generate salt: %w", err)
	}
	release := acquire()
	key := argon2.IDKey([]byte(plain), salt, iterations, memoryKiB, lanes, keyLen)
	release()
	return encode(salt, key, memoryKiB, iterations, lanes), nil
}

func encode(salt, key []byte, m, t uint32, p uint8) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, m, t, p, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// Verify reports whether plain matches the PHC string encoded. The
// comparison is constant-time. A malformed hash is an error
// (ErrMalformedHash), never a silent "no match", so a corrupt row is
// noticed; a wrong password is (false, nil).
func Verify(plain, encoded string) (bool, error) {
	salt, want, m, t, p, err := decode(encoded)
	if err != nil {
		return false, err
	}
	release := acquire()
	got := argon2.IDKey([]byte(plain), salt, t, m, p, uint32(len(want)))
	release()
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func decode(encoded string) (salt, key []byte, m, t uint32, p uint8, err error) {
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, ErrMalformedHash
	}
	var version int
	if n, scanErr := fmt.Sscanf(parts[2], "v=%d", &version); scanErr != nil || n != 1 || version != argon2.Version {
		return nil, nil, 0, 0, 0, ErrMalformedHash
	}
	var lanes32 uint32
	if n, scanErr := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &lanes32); scanErr != nil || n != 3 {
		return nil, nil, 0, 0, 0, ErrMalformedHash
	}
	if m == 0 || m > maxMemoryKiB || t == 0 || t > maxIterations || lanes32 == 0 || lanes32 > uint32(maxLanes) {
		return nil, nil, 0, 0, 0, ErrMalformedHash
	}
	p = uint8(lanes32)
	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) == 0 {
		return nil, nil, 0, 0, 0, ErrMalformedHash
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) == 0 {
		return nil, nil, 0, 0, 0, ErrMalformedHash
	}
	return salt, key, m, t, p, nil
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// DummyVerify burns the same CPU and memory as a real Verify, against a
// hash nobody can match. The login path calls it when the account does
// not exist (or cannot log in) so the response time does not reveal which
// usernames are real.
func DummyVerify(plain string) {
	dummyOnce.Do(func() {
		// A random throwaway password: the hash can never be matched.
		junk := make([]byte, 24)
		_, _ = rand.Read(junk)
		dummyHash, _ = Hash(string(junk))
	})
	_, _ = Verify(plain, dummyHash)
}

// ValidatePolicy enforces the password policy: MinLength..MaxLength
// characters, and not equal (case-insensitively) to username. Length is
// counted in Unicode code points, not bytes.
func ValidatePolicy(plain, username string) error {
	n := len([]rune(plain))
	switch {
	case n < MinLength:
		return ErrTooShort
	case n > MaxLength:
		return ErrTooLong
	case username != "" && strings.EqualFold(plain, username):
		return ErrEqualsUsername
	}
	return nil
}

// Temporary passwords are TempLength characters drawn uniformly from
// tempAlphabet: base62 without the visually ambiguous 0 O o 1 I l.
const (
	TempLength   = 20
	tempAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz"
)

// GenerateTemporary returns a fresh random temporary password. Sampling
// uses rejection so every character is equally likely (no modulo bias).
func GenerateTemporary() (string, error) {
	const n = len(tempAlphabet)
	// Largest multiple of n that fits in a byte; bytes at or above it are
	// rejected.
	const limit = 256 - (256 % n)

	out := make([]byte, 0, TempLength)
	buf := make([]byte, TempLength*2)
	for len(out) < TempLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("password: generate temporary password: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, tempAlphabet[int(b)%n])
			if len(out) == TempLength {
				break
			}
		}
	}
	return string(out), nil
}
