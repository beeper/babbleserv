# Implementation Details

## Users & Remote Servers are very similar

Matrix homeservers are responsible for distributing events to two groups:

- their own local users (sync)
- other homeservers (federation)

Babbleserv tries to manage each of these as similarly as the other to reduce code and data model complexity. Both users and servers track room memberships and membership changes. Both users and servers receive events via sync-like transactions. The main difference is users request events via the sync api and servers are pushed events via worker processes.

The best example of this is HERE where we fetch memberships for a user/server and the next batch of events - used in both sync and federation sending.

## No “Free” Relational Model

FoundationDB is a key value store, as such there are no foreign keys/etc. Instead data is just modelled as-is relevant to each use. For example there’s no “users” table. Instead there’s auth keys that map to userids and separate user id to room membership keys. Since auth tokens and memberships live in different databases there’s no trivial mechanism to simply “remove a user”.

This is definitely a disadvantage! But it’s also what makes FoubdationDB, and Babbleserv, so quick even as it scales in size. The data model is designed to minimise the need for such changes, and where absolutely required these are implemented using a queue of idempotent jobs that run/retry until they complete.

## Use of `gomatrixserverlib` Library

The `gomatrixserverlib` library is generally hard to use (everything is a custom type or an interface) and poorly, if at all, documented. As such Babbleserv tries to minimize it's usage while leaning on it for some critical functions: namely state resolution and event authorization. Unfortunately this means a bunch of boilerplate code exists to convert requests/responses between various types.

Babbleserv will not use it's more complicated functionality that was extracted from Dendrite as this requires implementing overly complex interfaces for lookup functions/etc that would be incompatible with the FoundationDB data model.
