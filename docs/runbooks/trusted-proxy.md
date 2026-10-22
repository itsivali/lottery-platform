# Runbook: client IP behind Render's load balancer

Per-IP rate limits (sign-up, login, refresh, SSO) and anonymous idempotency keys all key on the client
address. If that address is wrong, every player shares one limit (too coarse) or a client can mint unlimited addresses
(spoofable). The API therefore **refuses to start** with its secrets set unless `TRUSTED_PROXY_HEADER` is chosen.

## What was measured (2026-10-22)

With the `LOG_CLIENT_IP_HEADERS=true` diagnostic on the live service (it logs only the socket address and the address
headers), requests from one client (102.22.214.70) arrived as:

| Request | X-Forwarded-For | CF-Connecting-IP / True-Client-IP |
|---|---|---|
| plain | `102.22.214.70, 104.22.30.137, 10.31.213.134` | `102.22.214.70` |
| client sends `X-Forwarded-For: 9.9.9.9` | `9.9.9.9,102.22.214.70, 172.71.147.220, 10.31.213.134` | `102.22.214.70` |
| client sends `CF-Connecting-IP` / `True-Client-IP` | refused by Cloudflare with **403** before reaching the api | — |

- Render is behind **Cloudflare**; `X-Forwarded-For` is `client, cloudflare edge, render balancer`, appended to whatever
  the client sent. The rightmost entry (what `HOPS=1` used) is Render's internal balancer and **changes per request**
  (`10.29.246.145`, `10.27.133.182`, `10.31.213.134`, ...). That is why probe 1 failed: the 61 attempts were spread over
  several "clients" and none reached the limit. It was over-aggregation with rotation, not spoofing.
- **Decision: `TRUSTED_PROXY_HEADER=CF-Connecting-IP`.** It carries the real client and Cloudflare refuses requests that
  try to set it. `X-Forwarded-For` with `HOPS=3` would also work today, but would silently become spoofable if Render
  removed a hop. Render's internal health checks carry no such header and fall back to the socket address.
- `TestRenderBlueprint_TrustedProxySettingsFindTheRealClientInMeasuredRequests` replays the measured headers through
  the Blueprint's settings, so a change to them cannot quietly bring the problem back.
- If Render ever stops fronting services with Cloudflare, a client could send `CF-Connecting-IP` itself: re-measure
  after any platform change (switch the diagnostic on for a few minutes, then off).

## How the header is read

With `TRUSTED_PROXY_HEADER=X-Forwarded-For` and `TRUSTED_PROXY_HOPS=N`, the client is the **N-th entry from the right**;
with any other header its whole value must be one IP address. If the value is missing or not an IP, the socket address
is used instead.

## Verify after the first deploy (needs the live service URL)

1. **Spoof resistance.** The per-IP login budget is 60 attempts an hour. Send 61 attempts for *different* numbers (so the
   per-number budget does not trip first), each with a different forged leftmost entry:

   ```bash
   for i in $(seq 1 61); do
     curl -s -o /dev/null -w '%{http_code}\n' -X POST "$URL/v1/auth/login" \
       -H "Idempotency-Key: probe-$i" -H "X-Forwarded-For: 9.9.9.$i" -H 'Content-Type: application/json' \
       -d "{\"msisdn\":\"07000000$(printf %02d $i)\",\"password\":\"not a real password\",\"device\":{\"fingerprint\":\"probe\",\"platform\":\"android\"}}"
   done | sort | uniq -c
   ```

   Expected: sixty `401` and a `429` for the last. If all 61 are `401`, the address used is not a stable client address
   (spoofed, or a rotating proxy): **do not expose the API**; measure with the diagnostic and fix the setting.
2. **Not over-aggregated.** From a second network (for example a phone on mobile data) repeat 5 attempts. Expected `401`s,
   not `429`s. If you get `429` straight away, all clients share one address: the rightmost entry is a proxy. Check the
   chain (temporarily log `X-Forwarded-For`) and set `TRUSTED_PROXY_HOPS` to the position of the real client.
3. Record the observed headers and the chosen setting here (see "What was measured").

The same behaviour is covered locally by `TestApp_PerClientLimitsUseTheTrustedProxyEntryNotWhatTheClientClaims`.
