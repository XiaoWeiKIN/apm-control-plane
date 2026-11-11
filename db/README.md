# Relational Schema

The nine Agent Management tables are declared in the unified
[DataFox Admin migrations](https://github.com/datafoxlabs/datafox-schema/tree/main/admin/migrations)
using the storekit/migrator Go DSL.

Initialize the relational management schema in a fresh database (or an
existing Admin database without Agent Management tables) with a single
executor:

```sh
datafox-schema migrate up --domain=admin
```

The application requires a successful `admin_agent_management_schema`
migration history entry and validates its required tables at startup.
The server never runs AutoMigrate or implicitly adopts existing tables.
