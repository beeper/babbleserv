package databases

import (
	"context"
)

type migration struct {
	name    string
	handler func(context.Context) error
}

func (d *Databases) RunMigrations() {
	return
}

func (d *Databases) GetMigrations() []migration {
	return []migration{
		migration{"Migration01PopulateRoomVersionEventTups", d.Migration01PopulateRoomVersionEventTups},
	}
}

// Early stage dev migration, here sa an example for perfoming data migrations. Once completed we
// can remove the hack in `types.BytesToEventTup`.
// Migrations must function be self-contained, resumable iterators. Only one migration process is
// ever running at once, they run in the order defined above in `GetMigrations`. Each must complete
// by returning nil before the next starts. They should be prepared for context cancelations if the
// process is being restarted, and resume where were.
func (d *Databases) Migration01PopulateRoomVersionEventTups(ctx context.Context) error {
	// Iterate all events, tracking our position
	// Get event version (idToVersion)
	// Get events.KeyForRoomVersion, check if full tup, write if not
	// ditto for KeyForLocalRoomVersion
	return nil
}
