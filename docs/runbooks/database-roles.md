# Runbook: database roles on a managed Postgres (Render)

Role separation (`app_migrator` owns objects; `app_api`, `app_worker`, `app_readonly` have least privilege) needs a
database user that may create roles. Render's default database user is **not a superuser** and the provider does not
document whether it has `CREATEROLE`. What we verified locally by running the migrations as a non-superuser:

| Migrating user can | Result |
|---|---|
| create roles (`CREATEROLE`) | Roles are created, objects end up owned by `app_migrator`, grants apply. `GRANT ... WITH SET TRUE` is required on PostgreSQL 16+ and is in migration 00002. |
| not create roles | Migrations **succeed** in *single-role mode*: a `WARNING` is logged, role grants and ownership are skipped, and the API connects as the one database user. |

Single-role mode is a weaker deployment, not a broken one. The invariants that matter most are triggers and constraints and
hold for every role, including the owner: append-only ledger / wallet transactions / audit log, balanced journals,
wallet = sum of transactions, the 4-plays-per-day cap, one-time draw seed. What is lost is defence in depth: the API
process could in principle `INSERT`/`UPDATE` tables it should only read, and the worker/API/read-only split is not enforced.

## Check on the real instance

```sql
-- as the Render database user
SELECT rolname, rolsuper, rolcreaterole FROM pg_roles WHERE rolname = current_user;
SELECT rolname FROM pg_roles WHERE rolname LIKE 'app\_%';   -- expect 4 rows if roles were created
```

The deploy log of the first migration shows either no warning (roles created) or
`running in SINGLE-ROLE MODE`. If it shows the warning and you want role separation, ask Render support for `CREATEROLE`
or create the four roles once as a privileged user, then redeploy (the migrations pick them up).

## Moving to separate roles later

Create login roles that are members of `app_api` / `app_worker` / `app_readonly` (passwords in Render secrets, not in the
repository), point the api at the `app_api` login and the worker at `app_worker`. Nothing in the schema changes.
