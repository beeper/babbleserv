package util

import (
	"encoding/base32"
	"encoding/base64"
)

func Base64Encode(b []byte) string {
	return base64.RawStdEncoding.EncodeToString(b)
}

func Base64Decode(s string) ([]byte, error) {
	return base64.RawStdEncoding.DecodeString(s)
}

func Base64EncodeURLSafe(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func Base64DecodeURLSafe(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func Base32HexEncode(b []byte) string {
	return base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

func Base32HexDecode(s string) ([]byte, error) {
	return base32.HexEncoding.WithPadding(base32.NoPadding).DecodeString(s)
}
