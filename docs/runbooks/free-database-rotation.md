# Runbook: recreating the free Render Postgres before it expires

Until the database is upgraded to a paid plan, `.github/workflows/db-rotate.yml` keeps it alive. Render's free Postgres
**expires 30 days after creation** (then 14 days' grace to upgrade, then deletion), keeps **no backups**, and a
workspace may have **only one** free instance, so a new one cannot be created next to the old one.

## What runs

Daily at 01:00 Nairobi the workflow checks `expiresAt` of `lottery-db`. When 3 days or fewer remain it rotates:

1. **freeze**: suspend `lottery-api` so nothing is written after the backup (top-ups, sign-ups and jobs stop; a few
   minutes of downtime at night).
2. **backup**: `pg_dump` (Postgres 18 client), restore it into a throwaway Postgres, require identical row counts in
   every table, encrypt it (AES-256, PBKDF2) with `DB_BACKUP_PASSPHRASE` and keep it as the run's `db-backup` artifact
   for 14 days. The repository is public: the artifact is only safe because it is encrypted.
3. **replace**: delete the old instance, create a new free one with the same name, version, region, database name,
   user and IP allow list, restore, require the same row counts again, set the service's `DATABASE_URL` to the new
   internal URL, resume and deploy, and wait for `/readyz`.

The new instance gets a fresh 30 days. It runs in **single-role mode** (the dump is restored without owners or grants;
see `database-roles.md`): the api already connects as the one database user, and every trigger and constraint still
holds. Migrations stay at the restored version, so the api's migrate-on-start is a no-op.

Once the database is upgraded (`plan` is no longer `free`), the workflow does nothing; it can then be deleted.

## Setup (once)

The workflow uses the `database` environment (runs from `main` only, no approval, so the schedule can act unattended):

```
gh secret set RENDER_API_KEY --env database < ~/.config/lottery-platform/render.key
gh secret set DB_BACKUP_PASSPHRASE --env database          # paste a long random passphrase
```

**Keep `DB_BACKUP_PASSPHRASE` in the password manager.** Without it the backups cannot be decrypted.

## Manual runs (Actions → db-rotate → Run workflow)

- `check`: report plan and days left.
- `dry-run`: back up and verify the restore, keep the encrypted artifact; nothing on Render changes and the api keeps
  running (row counts may then differ by the job-queue rows written meanwhile; that is reported, not an error).
- `rotate`: rotate now, whatever the expiry.
- `recover`: see below.

## If a rotation fails

- **Before the old database was deleted** (backup or verification failed): the workflow resumes the api on the old
  database. Fix the cause and run `rotate` again.
- **After the delete** (create, restore or switch failed): the api stays suspended and the run summary names the run
  id. If a new, partly restored `lottery-db` exists, delete it in the dashboard first, then run the workflow with
  `mode=recover` and `recover_run_id=<that run id>`: it decrypts that run's backup, creates the instance, restores,
  checks the row counts and switches the service. The backup artifact is kept for 14 days.
- By hand, with the artifact downloaded: `openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 -in db.dump.enc -out db.dump`
  and `pg_restore --no-owner --no-privileges --exit-on-error -d <new external URL> db.dump`.

## Notes

- The Blueprint declares `lottery-db` with `fromDatabase`; the workflow sets `DATABASE_URL` explicitly after a rotation.
  After the first real rotation, check the Blueprint still shows as in sync in the dashboard.
- The script is `scripts/rotate-free-db.sh`. History:
  - 2026-10-22 `dry-run` (Actions run 39776895525): 38 tables verified; the artifact decrypts with the saved passphrase.
  - 2026-10-22 first `rotate`, supervised (run 39777173649): `dpg-dbcagf2fngtc739irh70-a` → `dpg-dbcvu9ivcj2c73eqb410-a`,
    3m25s end to end, about 2.5 minutes of api downtime (suspended 12:18:16, ready 12:21:09 UTC); 38 tables with
    identical row counts; migrations a no-op at version 15; the test account and its KYC record intact; Blueprint
    still in sync; new instance in single-role mode (no `app_*` roles), all 24 triggers present. New expiry 2026-11-21.
