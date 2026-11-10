# Control Plane database schema

The PostgreSQL/MySQL Control Plane schema is defined by the Go DSL in
[datafox-schema/controlplane/migrations](https://github.com/datafoxlabs/datafox-schema/tree/main/controlplane/migrations).

This repository does not maintain its own schema migrations. Initialize a fresh
database with a single-executor `datafox-schema migrate up --domain=controlplane`
job **before** starting the service. The runtime checks the
`controlplane_init_schema` success marker.

If any Control Plane business table already exists without a successful
migration record, initialization fails. There is no database takeover,
automatic repair or data deletion.
