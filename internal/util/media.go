package util

func ValidMediaID(mediaID string) bool {
	if len(mediaID) == 0 || len(mediaID) > 255 {
		return false
	}
	for _, char := range mediaID {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}
