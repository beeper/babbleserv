package debug

import (
	"net/http"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

	"github.com/beeper/babbleserv/internal/util"
)

type positionsWithTime struct {
	DatabaseName string
	Versionstamp tuple.Versionstamp
	UTCTime      time.Time
}

func (d *DebugRoutes) DebugGetIteratorPositions(w http.ResponseWriter, r *http.Request) {
	positions, err := d.db.System.GetAllIteratorPositions(r.Context())
	if err != nil {
		util.ResponseErrorUnknownJSON(w, r, err)
		return
	}

	posWithTime := make(map[string]positionsWithTime, len(positions))
	for name, pos := range positions {
		var sourceDB string
		switch name {
		case "EventsIteratorPositions":
			sourceDB = "rooms"
		case "DeviceChangeIteratorPositions":
			sourceDB = "accounts"
		case "ProfileChangeIteratorPositions":
			sourceDB = "accounts"
		default:
			sourceDB = "unknown"
		}

		var t time.Time
		switch sourceDB {
		case "accounts":
			t, err = d.db.Accounts.GetTimeForVersion(r.Context(), pos)
			if err != nil {
				util.ResponseErrorUnknownJSON(w, r, err)
				return
			}
		}

		posWithTime[name] = positionsWithTime{
			DatabaseName: sourceDB,
			Versionstamp: pos,
			UTCTime:      t,
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Positions any
	}{posWithTime})
}
