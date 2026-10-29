package events

import (
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/stretchr/testify/assert"
	"maunium.net/go/mautrix/id"
)

func TestOrderByVersion(t *testing.T) {
	at := func(transaction byte, user uint16) tuple.Versionstamp {
		return tuple.Versionstamp{TransactionVersion: [10]byte{9: transaction}, UserVersion: user}
	}
	eventIDs := []id.EventID{"$late", "$batchSecond", "$batchFirst", "$unstored", "$tieB", "$tieA"}
	versions := []tuple.Versionstamp{at(3, 0), at(2, 1), at(2, 0), {}, at(1, 0), at(1, 0)}
	assert.Equal(t,
		[]id.EventID{"$unstored", "$tieA", "$tieB", "$batchFirst", "$batchSecond", "$late"},
		OrderByVersion(eventIDs, versions),
	)
	assert.Equal(t, id.EventID("$late"), eventIDs[0], "the events keep their order")
}
