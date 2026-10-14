package util_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

func TestParseRequestJSON(t *testing.T) {
	type request struct {
		Content map[string]any `json:"content"`
	}
	for body, expected := range map[string]*mautrix.RespError{
		`{"content":{"a":1}}`:               nil,
		` {"content":{}} `:                  nil,
		`{"content":{"a":1,"a":2}}`:         nil,
		`{"content":{},"content":{}}`:       nil,
		`{"content":{"a":[{"b":1,"b":1}]}}`: nil,
		`null`:                              &mautrix.MBadJSON,
		`{"content":[]}`:                    &mautrix.MBadJSON,
		``:                                  &mautrix.MNotJSON,
		`{"content":`:                       &mautrix.MNotJSON,
		`{"content":{}}{}`:                  &mautrix.MNotJSON,
		`{"content":{}} x`:                  &mautrix.MNotJSON,
		`{"content":{},"content":{}}{}`:     &mautrix.MNotJSON,
		`{"content":{},"content":{}} x`:     &mautrix.MNotJSON,
		`{"content":{},"content":`:          &mautrix.MNotJSON,
		`{"content":{},"content":[]}`:       &mautrix.MBadJSON,
	} {
		_, respErr := util.ParseRequestJSON[request](httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		if expected == nil {
			assert.Nil(t, respErr, body)
		} else {
			require.NotNil(t, respErr, body)
			assert.Equal(t, expected.ErrCode, respErr.ErrCode, body)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"content":{"a":"`+strings.Repeat("x", 100)+`"}}`))
	req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, 64)
	_, respErr := util.ParseRequestJSON[request](req)
	require.NotNil(t, respErr)
	assert.Equal(t, mautrix.MTooLarge.ErrCode, respErr.ErrCode)
}

func TestParseRequestJSONDuplicateValuesReplace(t *testing.T) {
	type request struct {
		Content    map[string]any               `json:"content"`
		Signatures map[string]map[string]string `json:"signatures"`
	}
	body := `{"content":{"old":1},"content":{"new":2},"signatures":{"a":{"key":"old"}},"signatures":{"b":{"key":"new"}}}`
	req, respErr := util.ParseRequestJSON[request](httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	require.Nil(t, respErr)
	assert.Equal(t, map[string]any{"new": float64(2)}, req.Content)
	assert.Equal(t, map[string]map[string]string{"b": {"key": "new"}}, req.Signatures)
}

func TestParseRequestJSONRawContent(t *testing.T) {
	type request struct {
		Content json.RawMessage `json:"content"`
	}
	for body, expected := range map[string]string{
		`{"content":{ "a": 1 }}`: `{ "a": 1 }`,
		`{"content":{"a":1,"\u0061":2,"nested":[{"x":1,"x":2}],"n":9007199254740993}}`: `{"a":2,"n":9007199254740993,"nested":[{"x":2}]}`,
	} {
		req, respErr := util.ParseRequestJSON[request](httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		require.Nil(t, respErr, body)
		assert.Equal(t, expected, string(req.Content), body)
	}
}

func TestParseRequestJSONEvent(t *testing.T) {
	body := `{
		"type":"m.room.message",
		"type":"m.room.member",
		"state_key":"@bob:example.com",
		"content":{"membership":"join","displayname":"Bob","membership":"ban"},
		"signatures":{"a.example.com":{"ed25519:1":"a"}},
		"signatures":{"b.example.com":{"ed25519:1":"b"}}
	}`
	ev, respErr := util.ParseRequestJSON[types.Event](httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	require.Nil(t, respErr)
	assert.Equal(t, event.StateMember, ev.Type)
	assert.Equal(t, `{"displayname":"Bob","membership":"ban"}`, string(ev.Content))
	assert.Equal(t, event.MembershipBan, ev.Membership())
	assert.Equal(t, map[string]map[string]string{"b.example.com": {"ed25519:1": "b"}}, ev.Signatures)
}

func TestParseOptionalRequestJSON(t *testing.T) {
	type request struct {
		Reason string `json:"reason"`
	}
	newRequest := func(body string) *http.Request {
		return httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	}

	for _, body := range []string{``, `null`} {
		req, respErr := util.ParseOptionalRequestJSON[request](newRequest(body))
		assert.Nil(t, respErr, body)
		assert.Equal(t, request{}, req, body)
	}

	req, respErr := util.ParseOptionalRequestJSON[request](newRequest(`{"reason":"bye"}`))
	assert.Nil(t, respErr)
	assert.Equal(t, request{Reason: "bye"}, req)

	req, respErr = util.ParseOptionalRequestJSON[request](newRequest(`{"reason":"a","reason":"b"}`))
	require.Nil(t, respErr)
	assert.Equal(t, request{Reason: "b"}, req)

	for body, expected := range map[string]*mautrix.RespError{
		` `:          &mautrix.MNotJSON,
		`{"reason":`: &mautrix.MNotJSON,
		`[]`:         &mautrix.MBadJSON,
	} {
		_, respErr := util.ParseOptionalRequestJSON[request](newRequest(body))
		require.NotNil(t, respErr, body)
		assert.Equal(t, expected.ErrCode, respErr.ErrCode, body)
	}
}

func TestURLParamsArePercentDecoded(t *testing.T) {
	type ids struct {
		roomID  id.RoomID
		eventID id.EventID
		userID  id.UserID
	}
	var got ids
	router := chi.NewRouter()
	router.Get("/state_ids/{roomID}/{eventID}/{userID}", func(w http.ResponseWriter, r *http.Request) {
		got = ids{util.RoomIDFromRequestURLParam(r, "roomID"), util.EventIDFromRequestURLParam(r, "eventID"), util.UserIDFromRequestURLParam(r, "userID")}
	})
	want := ids{"!abc:example.org", "$ev/ent+id", "@alice:example.org"}
	for _, path := range []string{
		// As Synapse quotes path arguments
		"/state_ids/%21abc%3Aexample.org/%24ev%2Fent%2Bid/%40alice%3Aexample.org",
		"/state_ids/!abc:example.org/$ev%2Fent+id/@alice:example.org",
	} {
		got = ids{}
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, want, got, path)
	}

	var stateKey string
	router.Get("/state/{stateKey}", func(w http.ResponseWriter, r *http.Request) {
		stateKey = util.StateKeyFromRequestURLParam(r, "stateKey")
	})
	for path, want := range map[string]string{
		"/state/%40alice%3Aexample.org": "@alice:example.org",
		"/state/@alice:example.org":     "@alice:example.org",
		"/state/100%25":                 "100%",
		"/state/a%2541":                 "a%41",
		"/state/a%2541%2Fb":             "a%41/b",
	} {
		stateKey = ""
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, want, stateKey, path)
	}

	var userID id.UserID
	router.Get("/presence/{userID}", func(w http.ResponseWriter, r *http.Request) {
		userID = util.UserIDFromRequestURLParam(r, "userID")
	})
	for path, want := range map[string]id.UserID{
		"/presence/@100%25:example.org":     "@100%:example.org",
		"/presence/%40100%25%3Aexample.org": "@100%:example.org",
	} {
		userID = ""
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, want, userID, path)
	}
}
