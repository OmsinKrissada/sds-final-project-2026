package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Hash format: pbkdf2-sha256$<iter>$<b64 salt>$<b64 key>
// Base64 is unpadded standard encoding (RawStdEncoding).
const (
	pwIter    = 60000
	pwSaltLen = 16
	pwKeyLen  = 32
	pwPrefix  = "pbkdf2-sha256"
)

var b64 = base64.RawStdEncoding

// hashPassword returns a new encoded hash of password with a random salt.
func hashPassword(password string) (string, error) {
	salt := make([]byte, pwSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pwIter, pwKeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", pwPrefix, pwIter, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// verifyPassword reports whether password matches the encoded hash. Malformed
// hashes simply do not verify.
func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pwPrefix {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := b64.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}
