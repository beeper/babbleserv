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
}

// IngestTarget is an event a room's ingest job publishes once the state it needs is stored
type IngestTarget struct {
	Kind IngestTargetKind `msgpack:"knd"`
	// As signed and verified; its ID comes from the record's key
	Event *Event `msgpack:"evt"`
	// The origin of a PDU, the servers to send a join through, first the one whose template it is
	Servers  []string `msgpack:"srv"`
	Enqueued int64    `msgpack:"enq"`
	// A send_join may have been accepted, so refusing a later one does not mean the join was refused
	MayHaveLanded bool `msgpack:"mhl,omitempty"`
	// JoinTimeout counts from the first send_join
	FirstSent int64 `msgpack:"fst,omitempty"`
	// Failed rounds, not counting the job ending; the last, at FailedAt, holds it back for a retry pause
	Failures int   `msgpack:"fls,omitempty"`
	FailedAt int64 `msgpack:"fat,omitempty"`
	// The bytes a PDU counts for in its room's queue
	Size int `msgpack:"sz,omitempty"`
	// Fresh make_join joins that replaced ones a resident refused
	Replacements int `msgpack:"rpl,omitempty"`
	// Pulled by get_missing_events rather than sent here, so its own prev events are not, bounding pulls
	Pulled bool `msgpack:"pld,omitempty"`
}

type IngestQueue struct {
	PDUs  int64
	Bytes int64
}

func (q IngestQueue) Add(other IngestQueue) IngestQueue {
	return IngestQueue{PDUs: q.PDUs + other.PDUs, Bytes: q.Bytes + other.Bytes}
}

func (q IngestQueue) Full(maxPDUs int, maxBytes int64) bool {
	return q.PDUs >= int64(maxPDUs) || q.Bytes >= maxBytes
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
