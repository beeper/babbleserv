# Babbleserv Data Model

**NOTE: this data model is very much in-flux and subject to significant change.**

All Babbleserv persistent data lives in FoundationDB (FDB). Data is divided into distinct databases that do not share anything and thus can be placed on separate FDB clusters. Note this should only ever be needed at extreme scale, but it makes sense to split the data now rather than later. Each database directory uses distinct prefixes so a single FDB cluster (or node) may be used safely.

All keys share a single namespace in FDB, data is separated by prefixes called directories. Each database contains a number of directories and each directory contains sub-prefixes for things like indices/etc. See each database doc for details.

Databases:

- [**Rooms**](./data-model-rooms.md) (rooms, events, receipts, aliases, room/user directories)
    - persistent forever data
    - one position token, one FDB range per room per sync
- [**Accounts**](./data-model-accounts.md) (logins, auth tokens, devices, account data)
    - persistent forever data
    - one position token, one FDB range per sync
- [**Transitory**](./data-model-transitory.md) (to-device, typing, device/presence updates)
    - ephemeral data
    - one position token, one FDB range per room per sync plus one for to-device

## Databases Implementation

Each database contains multiple directories, which look like:

```go
eventsDir, err := directory.CreateOrOpen(db, []string{"roomsdb_events"}, nil)
```

The name combines the database and directory, any length is fine here (FDB maps these to short keys). Within each directory any number of subspaces may exist - **the subspace keys should be as short as possible** to optimise reads:

```go
ByID := eventsDir.Sub("id"),  // event by ID
```

Each subspace then contains tuple keys mapping to byte or tuple values. The database struct should ideally provide wrappers to read/write both keys and values for each subspace (exception: single byte values eg where value is just an event ID). The the `internal/databases/rooms/events/events.go` file for examples of this.
