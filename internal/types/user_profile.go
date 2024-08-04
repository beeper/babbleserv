package types

import "github.com/vmihailenco/msgpack/v5"

type UserProfile struct {
	DisplayName string `json:"displayname" msgpack:"dn"`
	AvatarURL   string `json:"avatar_url" msgpack:"au"`

	Custom map[string]any `json:"-" msgpack:"cu"` // MSC4133 placeholder
}

func NewUserProfileFromBytes(b []byte) (*UserProfile, error) {
	var u UserProfile
	if err := msgpack.Unmarshal(b, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

func MustNewUserProfileFromBytes(b []byte) *UserProfile {
	r, err := NewUserProfileFromBytes(b)
	if err != nil {
		panic(err)
	}
	return r
}

func (p *UserProfile) ToMsgpack() []byte {
	if bytes, err := msgpack.Marshal(p); err != nil {
		panic(err)
	} else {
		return bytes
	}
}

func (p *UserProfile) ToMembershipContent() map[string]any {
	content := make(map[string]any, 2)
	for k, v := range p.Custom {
		content[k] = v
	}
	if p.DisplayName != "" {
		content["displayname"] = p.DisplayName
	}
	if p.AvatarURL != "" {
		content["avatar_url"] = p.AvatarURL
	}
	return content
}
