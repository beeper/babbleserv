package types

import "errors"

var ErrEventNotFound = errors.New("event not found")
var ErrAlreadyExists = errors.New("event already exists")
var ErrEventRedacted = errors.New("event has been redacted")

var ErrProfileNotChanged = errors.New("profile is unchanged")
