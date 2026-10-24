# Runbook: secrets and key custody

| Variable | Purpose | If lost | Rotation |
|---|---|---|---|
| `DATA_KEY` / `DATA_KEYS` | Encrypts national ID, name and date of birth in `kyc.kyc_checks` | **Stored KYC data is unrecoverable** | Add a new key id to `DATA_KEYS` (`1:<old>,2:<new>`); new rows use the highest id, old rows stay readable. Re-encrypt in a job, then retire the old key. |
| `IDENTITY_KEY` | HMAC for the one-account-per-national-ID lookup | Duplicate detection resets; two accounts could verify the same ID | Do not rotate without a re-hash migration. |
| `AUTH_KEY` | Root for the password pepper and device-fingerprint HMACs | **Every password stops verifying**; players must reset | Do not rotate without a plan: changing it invalidates all stored password hashes. |
| `TOKEN_SEED` | Signs access tokens | All sessions end | Rotate at will; users sign in again. |

- On Render these are `generateValue: true` environment variables. **Copy `AUTH_KEY`, `DATA_KEY` and `IDENTITY_KEY` into
  the operator's password manager the day the service is created.** Deleting the service deletes them (it happened on
  2026-10-24, with only a test account affected): without the old values, passwords (`AUTH_KEY` pepper), KYC fields
  (`DATA_KEY`) and the one-account-per-ID lookup (`IDENTITY_KEY`) of existing accounts can no longer be checked.
- None of these are ever committed. The database never holds a key, so no database role can decrypt KYC data on its own.
- The sign-in client ids (`GOOGLE_CLIENT_ID`, `APPLE_CLIENT_ID`) are `sync: false` and entered once in the dashboard. Unset means the feature fails closed.
