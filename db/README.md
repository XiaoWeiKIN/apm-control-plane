# Control Plane database schema

The Control Plane PostgreSQL/MySQL schema is maintained by
[datafox-schema/controlplane/migrations](https://github.com/datafoxlabs/datafox-schema/tree/main/controlplane/migrations).

No local Goose migrations are maintained. Initialize a new database or adopt a
completed legacy Goose v4 database with a single-executor
`datafox-schema migrate up --domain=controlplane` before starting this service.
The runtime validates the `controlplane_init_schema` success marker.

Existing Goose migration history and business rows are preserved during adoption.
Do not remove or manually rewrite a production migration history table.
