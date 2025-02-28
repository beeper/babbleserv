package types

import (
	"time"

	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"
)

type Media struct {
	// Both provided from the key
	ServerName string `msgpack:"-"`
	MediaID    string `msgpack:"-"`

	StoreKey  string `msgpack:"sk"`
	StorePath string `msgpack:"sp"`

	Size        int64  `msgpack:"sz,omitempty"`
	ContentType string `msgpack:"ty,omitempty"`

	Sender id.UserID `msgpack:"snd"`

	CreatedAt  time.Time `msgpack:"ct"`
	UploadedAt time.Time `msgpack:"ut,omitempty"`
}

func NewMediaFromBytes(b []byte, serverName, mediaID string) (*Media, error) {
	var m Media
	if err := msgpack.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m.ServerName = serverName
	m.MediaID = mediaID
	return &m, nil
}

func MustNewMediaFromBytes(b []byte, serverName, mediaID string) *Media {
	if m, err := NewMediaFromBytes(b, serverName, mediaID); err != nil {
		panic(err)
	} else {
		return m
	}
}

func NewMedia(serverName, mediaID, storeKey string, sender id.UserID) *Media {
	return &Media{
		ServerName: serverName,
		MediaID:    mediaID,
		StoreKey:   storeKey,
		StorePath:  serverName + "/" + mediaID,
		Sender:     sender,
		CreatedAt:  time.Now().UTC(),
	}
}

func (m *Media) ToMsgpack() []byte {
	if b, err := msgpack.Marshal(m); err != nil {
		panic(err)
	} else {
		return b
	}
}

func (m *Media) ToContentURI() id.ContentURI {
	return id.ContentURI{
		Homeserver: m.ServerName,
		FileID:     m.MediaID,
	}
}
