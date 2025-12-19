# Babbleserv Databases

**NOTE: this data model is very much in-flux and subject to significant change.**

All Babbleserv persistent data lives in FoundationDB (FDB). Data is divided into distinct databases that do not share anything and can be placed either on a single FDB instance/cluster or an instance/cluster per database.

Databases:

- **Rooms** (rooms, events, receipts, aliases, room/user directories)
    - persistent forever data
    - one position token, one FDB range per room per sync
- **Accounts** (logins, auth tokens, devices, account data)
    - persistent for user lifetime data
    - one position token, one FDB range per sync
- **Transient** (device list, typing & presence updates)
    - ephemeral/transient/temporary data
    - one position token, one FDB range per room per sync + one for to-device
- **Media** (media)
    - media repo data
- **System**
    - for Babbleserv internal data, must always exist


## Databases Implementation

Quick notes on FDB:

- keys + values are bytes
- keys are stored in lexicographical order
- so data is stored by prefix

---

Each top level database owns it's own prefix within FoundationDB. May contain 1+ directories which
occupy a sub-prefix. Databases + directories store data under struct fields with foundationdb type 
`subspace.Subspace`. Example:

- `AccountsDatabase`: `internal/databases/accounts/accounts.go`
    - `DevicesDirectory`: `internal/databases/accounts/devices/devices.go`
        - `DevicesDirectory.userDevices`: `(UserID, DeviceID)` -> `types.Device`

Unlike sql keys/values have no validation or type support (each prefix is just a `subspace.Subspace`)
so we want the encode/decode stage to happen at the lowest level so we get strong types up the stack.
This is structured like so:

- `AccountsDatabase`
    - public API for making changes (`GetUserDevice`, etc)
    - creates relevant transaction (`util.DoReadTransaction`, etc)
    - methods often make calls through to `.devices`:
    - `DevicesDirectory`
        - exposes methods that work with transactions (`TxnGetDevice`, etc)
        - these handle the translation of keys + values between go types / bytes

### Transactions

All code execution within a transaction is wrapped by FoundationDB such that it may be re-executed
in case of serilization errors (opportunistic concurrency). As such there are rules/guidelines:

- it's ok to `panic()`: https://pkg.go.dev/github.com/apple/foundationdb/bindings/go/src/fdb#hdr-On_Panics
- no/minimal goroutines (exception being sync): https://pkg.go.dev/github.com/apple/foundationdb/bindings/go/src/fdb#hdr-Transactions_and_Goroutines

## Iterators (for cross-database transactions)

Babbleserv does not implement any distributed transaction logic (three phase commit/simlar). Instead we use an iterator pattern. Iterators are just workers that paginate a range in one database which indicates data to be changed in another database. Iterators persist their positions and thus provide at least once guarantees to data they need to process.

- `EventsIterator`
    - iterates all new events
    - wakes up relevant federation senders for new events
- `ProfileChangeIterator`
    - accounts database profiles -> rooms database member events
    - sends updated m.room.member events when a user changes their profile
    - clears old profile changes as it goes
- `DeviceChangeIterator`
    - accounts database device changes -> transient database to-device
    - sends device list changes via to-device when a user changes their cross-signing or device keys
    - clears old device changes as it goes

Iterators are a core part of Babbleserv and power outgoing federation, device list change propagation and more. They live in the workers package and all store positions in the system database. There is an admin API to view their state.


## Data Migrations

Data migrations are.. just iterators!
