package debug

import (
	"net/http"
	"time"

	"github.com/beeper/babbleserv/internal/types"
	"github.com/beeper/babbleserv/internal/util"
)

type positionsWithTime struct {
	DatabaseName string
	Version      string
	UTCTime      *time.Time
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
		case "PresenceChangeIteratorPositions":
			sourceDB = "transient"
		default:
			sourceDB = "unknown"
		}

		version := "unknown"
		var t *time.Time
		var err error
		switch sourceDB {
		case "accounts":
			t, err = d.db.Accounts.GetTimeForVersion(r.Context(), pos)
		case "rooms":
			t, err = d.db.Rooms.GetTimeForVersion(r.Context(), pos)
		case "transient":
			t, err = d.db.Transient.GetTimeForVersion(r.Context(), pos)
		}
		if err != nil {
			util.ResponseErrorUnknownJSON(w, r, err)
			return
		} else if t != nil {
			version = types.MustVersionstampToString(pos)
		}

		posWithTime[name] = positionsWithTime{
			DatabaseName: sourceDB,
			Version:      version,
			UTCTime:      t,
		}
	}

	util.ResponseJSON(w, r, http.StatusOK, struct {
		Positions any
	}{posWithTime})
}
