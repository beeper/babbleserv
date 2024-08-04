# Babbleserv Data Model: Transitory Database

The transitory database is responsible for the transitory specific pieces of the Matrix implementation: to-device events & device changes/lists. Items in this database are removed after the configured sync window: **everything in the devices database is considered ephemeral**.
