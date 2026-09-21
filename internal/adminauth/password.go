// Package adminauth owns the Web Admin credential domain:
// <relay>/config/admin.json — the admin username plus an Argon2id
// password hash — and the in-memory browser session authority built on
// top of it.
//
// It is a credential domain separate from the machine API bearer and the
// ephemeral descriptor bearer. The file is created once — atomically and
// never overwritten — by the daemon's first-run setup flow, and loaded
// and strictly validated on every start. Malformed or insecure content
// fails closed; plaintext passwords are never stored, logged, or
// returned.
package adminauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id work factors. Production uses ProductionParams;
// tests may substitute a cheaper Params through the explicit seam —
// persisted PHC records carry their own parameters, so verification cost
// is always bounded by parser limits regardless of the configured params.
type Params struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltBytes   int
	KeyBytes    int
}

// ProductionParams are the production Argon2id work factors: 64 MiB,
// 3 iterations, parallelism 2, 128-bit salt, 256-bit key.
var ProductionParams = Params{
	MemoryKiB:   64 << 10,
	Iterations:  3,
	Parallelism: 2,
	SaltBytes:   16,
	KeyBytes:    32,
}

// TestParams is the explicit cheap seam for tests. It stays inside the
// parser acceptance bounds so test-generated credentials verify.
var TestParams = Params{
	MemoryKiB:   8 << 10,
	Iterations:  1,
	Parallelism: 1,
	SaltBytes:   16,
	KeyBytes:    32,
}

// Parser acceptance bounds for a persisted PHC record. Anything outside
// is rejected before hashing — an absurd record must not turn credential
// verification into a denial of service.
const (
	phcVersion      = 19 // argon2 v=19
	minMemoryKiB    = 8 << 10
	maxMemoryKiB    = 1 << 20
	minIterations   = 1
	maxIterations   = 8
	minParallelism  = 1
	maxParallelism  = 8
	minSaltBytes    = 16
	maxSaltBytes    = 64
	minKeyBytes     = 32
	maxKeyBytes     = 64
	maxPasswordByte = 256
	minPasswordByte = 12
)

// Validate checks work-factor sanity before use.
func (p Params) Validate() error {
	if p.MemoryKiB < minMemoryKiB || p.MemoryKiB > maxMemoryKiB {
		return fmt.Errorf("argon2 memory %d KiB out of range %d..%d", p.MemoryKiB, minMemoryKiB, maxMemoryKiB)
	}
	if p.Iterations < minIterations || p.Iterations > maxIterations {
		return fmt.Errorf("argon2 iterations %d out of range %d..%d", p.Iterations, minIterations, maxIterations)
	}
	if p.Parallelism < minParallelism || p.Parallelism > maxParallelism {
		return fmt.Errorf("argon2 parallelism %d out of range %d..%d", p.Parallelism, minParallelism, maxParallelism)
	}
	if p.SaltBytes < minSaltBytes || p.SaltBytes > maxSaltBytes {
		return fmt.Errorf("argon2 salt %d bytes out of range %d..%d", p.SaltBytes, minSaltBytes, maxSaltBytes)
	}
	if p.KeyBytes < minKeyBytes || p.KeyBytes > maxKeyBytes {
		return fmt.Errorf("argon2 key %d bytes out of range %d..%d", p.KeyBytes, minKeyBytes, maxKeyBytes)
	}
	return nil
}

// ValidatePassword enforces the input contract: 12..256 bytes, valid
// UTF-8, no NUL, never trimmed — length is the policy.
func ValidatePassword(password string) error {
	if len(password) < minPasswordByte {
		return fmt.Errorf("password must be at least %d bytes", minPasswordByte)
	}
	if len(password) > maxPasswordByte {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordByte)
	}
	if strings.ContainsRune(password, 0) {
		return errors.New("password must not contain NUL")
	}
	for _, r := range password {
		if r == 0xFFFD {
			return errors.New("password must be valid UTF-8")
		}
	}
	return nil
}

// Hash produces a PHC-encoded Argon2id record for password under params.
func Hash(password string, params Params) (string, error) {
	if err := params.Validate(); err != nil {
		return "", err
	}
	salt := make([]byte, params.SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, params.Iterations,
		params.MemoryKiB, params.Parallelism, uint32(params.KeyBytes))
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		phcVersion, params.MemoryKiB, params.Iterations, params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// parsedPHC is a decoded PHC record.
type parsedPHC struct {
	params Params
	salt   []byte
	key    []byte
}

// parsePHC strictly decodes a stored record: exact field set, exact
// order, no duplicates, bounded parameters, strict base64.
func parsePHC(encoded string) (parsedPHC, error) {
	fail := func() (parsedPHC, error) {
		return parsedPHC{}, errors.New("malformed Argon2id PHC record")
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return fail()
	}
	if !strings.HasPrefix(parts[2], "v=") {
		return fail()
	}
	version, err := strconv.Atoi(parts[2][2:])
	if err != nil || version != phcVersion {
		return fail()
	}
	var mem, iter, par uint64
	seen := map[byte]bool{}
	for _, field := range strings.Split(parts[3], ",") {
		if len(field) < 3 || field[1] != '=' || seen[field[0]] {
			return fail()
		}
		seen[field[0]] = true
		v, err := strconv.ParseUint(field[2:], 10, 32)
		if err != nil {
			return fail()
		}
		switch field[0] {
		case 'm':
			mem = v
		case 't':
			iter = v
		case 'p':
			par = v
		default:
			return fail()
		}
	}
	if !seen['m'] || !seen['t'] || !seen['p'] {
		return fail()
	}
	p := Params{
		MemoryKiB:   uint32(mem),
		Iterations:  uint32(iter),
		Parallelism: uint8(par),
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return fail()
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return fail()
	}
	p.SaltBytes, p.KeyBytes = len(salt), len(key)
	if err := p.Validate(); err != nil {
		return fail()
	}
	return parsedPHC{
		params: p,
		salt:   salt,
		key:    key,
	}, nil
}

// Verify checks password against a stored PHC record with constant-time
// comparison. Malformed or out-of-bounds records fail closed.
func Verify(password, encoded string) bool {
	rec, err := parsePHC(encoded)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), rec.salt, rec.params.Iterations,
		rec.params.MemoryKiB, rec.params.Parallelism, uint32(len(rec.key)))
	return subtle.ConstantTimeCompare(got, rec.key) == 1
}
