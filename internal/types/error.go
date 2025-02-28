package types

import "errors"

var (
	ErrEventNotFound = errors.New("event not found")
	ErrAlreadyExists = errors.New("event already exists")
	ErrEventRedacted = errors.New("event has been redacted")

	ErrUserNotInRoom     = errors.New("user is not in this room")
	ErrUserNotFound      = errors.New("user not found")
	ErrTokenExpired      = errors.New("token is expired")
	ErrUserAlreadyExists = errors.New("username already exists")
	ErrProfileNotChanged = errors.New("profile is unchanged")
	ErrInvalidPassword   = errors.New("invalid password")
)
