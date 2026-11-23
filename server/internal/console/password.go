package console

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id with the OWASP minimum for it (19 MiB, 2 passes, 1 lane). The memory is the cost that matters in a pod of
// 256 MiB, so hashing runs at most [maxConcurrentHashes] at a time: the others wait, which is also what keeps a flood
// of sign-in attempts from using all the memory.
const (
	argonMemoryKiB      = 19 * 1024
	argonTime           = 2
	argonThreads        = 1
	argonKeyLen         = 32
	saltLen             = 16
	maxConcurrentHashes = 3
)

var hashSlots = make(chan struct{}, maxConcurrentHashes)

func hashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hashSlots <- struct{}{}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	<-hashSlots
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword compares in constant time. A hash it cannot read is a mismatch, not an error: the caller treats
// both the same.
func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var memory uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	// what the hash asks for is bounded: a row that was altered must not make the server allocate gigabytes
	if memory > 256*1024 || time > 10 || threads == 0 || threads > 8 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	hashSlots <- struct{}{}
	if len(want) > 1024 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want))) //nolint:gosec // bounded just above
	<-hashSlots
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked when the e-mail is unknown, so that an unknown address takes as long to answer as a wrong password.
var dummyHash = func() string {
	h, err := hashPassword("marpokat-dummy-password")
	if err != nil {
		panic("console: " + err.Error())
	}
	return h
}()
