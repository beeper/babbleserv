package types

import "errors"

var (
	ErrEventNotFound    = errors.New("event not found")
	ErrAlreadyExists    = errors.New("event already exists")
	ErrEventRedacted    = errors.New("event has been redacted")
	ErrStateUnavailable = errors.New("state not known at event")

	ErrUserNotInRoom      = errors.New("user is not in this room")
	ErrUserNotFound       = errors.New("user not found")
	ErrUserDeviceNotFound = errors.New("user device not found")

	ErrUIASessionNotFound = errors.New("UIA session not found")
	ErrUIASessionExpired  = errors.New("UIA session expired")
	ErrUIASessionMismatch = errors.New("UIA session mismatch")

	ErrTokenExpired      = errors.New("token is expired")
	ErrUserAlreadyExists = errors.New("username already exists")
	ErrInvalidPassword   = errors.New("invalid password")

	ErrRoomNotFound              = errors.New("room not found")
	ErrRoomTooLarge              = errors.New("the mandatory rows of this change exceed the transaction limit")
	ErrRoomAliasNotFound         = errors.New("room alias not found")
	ErrRoomAliasTaken            = errors.New("room alias taken")
	ErrRoomPublicationForbidden  = errors.New("room publication forbidden")
	ErrProfileDisplayNameTooLong = errors.New("profile display name exceeds search index limit")

	ErrInvalidPaginationToken = errors.New("invalid pagination token")
)
