package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"

	"railway/internal/platform/bloom"
)

// Hasher hashes passwords with argon2id into PHC strings.
type Hasher struct {
	MemoryKiB uint32
	Time      uint32
	Threads   uint8
	dummy     string
}

func NewHasher(mem, t uint32, threads uint8) *Hasher {
	h := &Hasher{MemoryKiB: mem, Time: t, Threads: threads}
	h.dummy, _ = h.Hash("dummy-password-for-unknown-users")
	return h
}

func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, h.Time, h.MemoryKiB, h.Threads, 32)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, h.MemoryKiB, h.Time, h.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Verify checks a password against a PHC string in constant time.
func (h *Hasher) Verify(password, phc string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var mem, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, mem, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// VerifyDummy burns the same CPU as a real verify, so response time does not
// reveal which accounts exist.
func (h *Hasher) VerifyDummy(password string) { h.Verify(password, h.dummy) }

var ErrWeakPassword = errors.New("password too weak")

// breached is a Bloom filter of known-leaked passwords. A production deployment
// loads a large list; this embeds the most common ones.
var breached = func() *bloom.Filter {
	f := bloom.New(1000, 0.001)
	for _, p := range []string{
		"password", "password1", "password12", "password123", "password1234", "1234567890", "123456789",
		"12345678", "qwertyuiop", "1q2w3e4r5t", "iloveyou12", "qwerty1234", "abcdefghij", "0987654321",
		"1111111111", "passw0rd12", "welcome123", "admin12345", "letmein123", "football12", "monkey1234",
		"dragon1234", "sunshine12", "princess12", "baseball12", "trustno1234", "superman12", "qwerty12345",
	} {
		f.Add(p)
	}
	return f
}()

// CheckPassword enforces the registration rules: at least 10 characters and not
// in the breached-password filter.
func CheckPassword(p string) error {
	if len(p) < 10 {
		return fmt.Errorf("%w: at least 10 characters", ErrWeakPassword)
	}
	if len(p) > 256 {
		return fmt.Errorf("%w: at most 256 characters", ErrWeakPassword)
	}
	if breached.MayContain(strings.ToLower(p)) {
		return fmt.Errorf("%w: found in a breached-password list", ErrWeakPassword)
	}
	return nil
}
