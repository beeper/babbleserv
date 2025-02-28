package util

import (
	"crypto/rand"
	"encoding/base32"
)

func GenerateRandomBytes(n int) []byte {
	b := make([]byte, n)
	_, err := rand.Read(b)
	// Note that err == nil only if we read len(b) bytes.
	if err != nil {
		panic(err)
	} else {
		return b
	}
}

// Generate random string suitable for use as an auth token/similar
func GenerateRandomString(n int) string {
	return Base64EncodeURLSafe(GenerateRandomBytes(n))
}

// As above but encoding using base32hex which is more human readable, ie for device IDs
func GenerateRandomStringBase32Hex(n int) string {
	return base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(GenerateRandomBytes(n))
}
