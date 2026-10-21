# Runbook: secrets and key custody

| Variable | Purpose | If lost | Rotation |
|---|---|---|---|
| `DATA_KEY` / `DATA_KEYS` | Encrypts national ID, name and date of birth in `kyc.kyc_checks` | **Stored KYC data is unrecoverable** | Add a new key id to `DATA_KEYS` (`1:<old>,2:<new>`); new rows use the highest id, old rows stay readable. Re-encrypt in a job, then retire the old key. |
| `IDENTITY_KEY` | HMAC for the one-account-per-national-ID lookup | Duplicate detection resets; two accounts could verify the same ID | Do not rotate without a re-hash migration. |
| `AUTH_KEY` | Root for the password pepper and device-fingerprint HMACs | **Every password stops verifying**; players must reset | Do not rotate without a plan: changing it invalidates all stored password hashes. |
| `TOKEN_SEED` | Signs access tokens | All sessions end | Rotate at will; users sign in again. |

- On Render these are `generateValue: true` environment variables. **Copy `DATA_KEY` and `IDENTITY_KEY` into the operator's
  password manager the day the service is created**; Render will not show a generated value you did not save.
- None of these are ever committed. The database never holds a key, so no database role can decrypt KYC data on its own.
- The national-registry gateway and sign-in client ids (`KYC_API_*`, `GOOGLE_CLIENT_ID`, `APPLE_CLIENT_ID`) are
  `sync: false` and entered once in the dashboard. Unset means the feature fails closed.
