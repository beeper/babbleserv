package types

import (
	"github.com/vmihailenco/msgpack/v5"
	"maunium.net/go/mautrix/id"
)

type IngestTargetKind string

const (
	IngestTargetJoin IngestTargetKind = "join"
	IngestTargetPDU  IngestTargetKind = "pdu"
)

// IngestJob is a room's ingest job, which exists while the room has targets to publish
type IngestJob struct {
	RoomID      id.RoomID `msgpack:"-"`
	RoomVersion string    `msgpack:"ver"`
	Created     int64     `msgpack:"crt"`
}

// IngestTarget is an event a room's ingest job publishes once the state it needs is stored
type IngestTarget struct {
	Kind IngestTargetKind `msgpack:"knd"`
	// As signed and verified, its ID taken from the record's key
	Event *Event `msgpack:"evt"`
	// The origin of a PDU, the servers to send a join through, first the one whose template it is
	Servers  []string `msgpack:"srv"`
	Enqueued int64    `msgpack:"enq"`
	// A send_join was sent that the resident server may have accepted, so refusing a later one
	// does not mean the join was refused
	MayHaveLanded bool `msgpack:"mhl,omitempty"`
	// Rounds whose work for the target failed, other than by the job ending
	Failures int `msgpack:"fls,omitempty"`
}

// IngestJoin is a local user's join to a room through the room's ingest job, until it publishes
type IngestJoin struct {
	EventID id.EventID     `msgpack:"eid"`
	Failure *IngestFailure `msgpack:"fail,omitempty"`
}

// IngestFailure is why a join target failed, as the user's /join answers it
type IngestFailure struct {
	Status  int    `msgpack:"sts"`
	ErrCode string `msgpack:"erc"`
	Message string `msgpack:"msg"`
}

func (f *IngestFailure) Error() string {
	return f.ErrCode + ": " + f.Message
}

func (j *IngestJob) ToMsgpack() []byte {
	return mustMsgpack(j)
}

func NewIngestJobFromBytes(b []byte, roomID id.RoomID) (*IngestJob, error) {
	var job IngestJob
	if err := msgpack.Unmarshal(b, &job); err != nil {
		return nil, err
	}
	job.RoomID = roomID
	return &job, nil
}

func (t *IngestTarget) ToMsgpack() []byte {
	return mustMsgpack(t)
}

func NewIngestTargetFromBytes(b []byte, eventID id.EventID) (*IngestTarget, error) {
	var target IngestTarget
	if err := msgpack.Unmarshal(b, &target); err != nil {
		return nil, err
	} else if target.Event == nil {
		return nil, ErrEventNotFound
	}
	target.Event.ID = eventID
	return &target, nil
}

func (j *IngestJoin) ToMsgpack() []byte {
	return mustMsgpack(j)
}

func NewIngestJoinFromBytes(b []byte) (*IngestJoin, error) {
	var join IngestJoin
	if err := msgpack.Unmarshal(b, &join); err != nil {
		return nil, err
	}
	return &join, nil
}

func mustMsgpack(v any) []byte {
	b, err := msgpack.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
