# Project Structure

Babbleserv is designed to lean on FoundationDB as much as possible. Think of it as a "thin" layer implementing Matrix data model and APIs on top of FoundationDBs consistency guarantees.

## Transactions

At the core of this lies transactions which FDB provides strong consistency guarantees for. By **evaluating event authorzation and state resolution within transactions** we simplify handling of both local and federated events significantly. See the `SendLocalEvents` and `SendFederatedEvents` in `internal/database/send_events.go` which implement auth + store in single transactions.

### Transaction Rules

Per the FoundationDB binding recommendations the following rules apply:

- transactions have a five second limit, this cannot be changed
- minimize or ideally don't use goroutines within transactions (sync is a good exception to this)
- use futures to prefetch data, transactions should roughly flow like:
    - iterate through all data we need, telling FDB what we'll need
    - then do any processing stage using the data
    
Ingesting federated events is a good example of this - all the network fetching of events we're missing has to happen before the transaction begins or we'll miss the five second deadline.


## Module Layout

### `internal/databases/*/`

- each represents a FDB cluster containing a logical group of sub-databases
- top level database transactions called by routes
- call through to the domain specific directories nested modules

#### `internal/databases/*/*/`

- individual database "directories" (FDB thing)
- group together common key prefix operations (ie events, users)
- not exported/available outside of database

### `internal/routes/`

- implement Matrix endpoints
- call through to database transactions

### `internal/federator/`

- federator?
