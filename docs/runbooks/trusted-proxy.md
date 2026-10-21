# Runbook: client IP behind Render's load balancer

Per-IP rate limits (sign-up, login, refresh, SSO) and anonymous idempotency keys all key on the client
address. If that address is wrong, every player shares one limit (too coarse) or a client can mint unlimited addresses
(spoofable). The API therefore **refuses to start** with its secrets set unless `TRUSTED_PROXY_HEADER` is chosen.

## What we know and don't know

- Render does not document a `True-Client-IP` header. Community reports on `X-Forwarded-For` disagree on whether Render
  appends to, or sets, the client's own header, and one report counts three proxies on some routes.
- Our rule is therefore the conservative one: with `TRUSTED_PROXY_HEADER=X-Forwarded-For` and `TRUSTED_PROXY_HOPS=N`,
  the client is the **N-th entry from the right**. Entries on the left are whatever the client sent and are never used.
  If the chain has fewer than N entries, or the entry is not an IP, the socket address is used instead.
- With `HOPS=1` the address can never be spoofed (it is the entry Render's own proxy added). In the worst case it is a
  proxy address and limits become coarse. Raise HOPS only after measuring, never to "fix" a spoofing test.

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

   Expected: sixty `401` and a `429` for the last. If all 61 are `401`, the header is being trusted from the client:
   **stop, set `TRUSTED_PROXY_HEADER=none` or fix the hop count, and do not expose the API.**
2. **Not over-aggregated.** From a second network (for example a phone on mobile data) repeat 5 attempts. Expected `401`s,
   not `429`s. If you get `429` straight away, all clients share one address: the rightmost entry is a proxy. Check the
   chain (temporarily log `X-Forwarded-For`) and set `TRUSTED_PROXY_HOPS` to the position of the real client.
3. Record the observed chain and the chosen HOPS in the open-decisions register (item 26).

The same behaviour is covered locally by `TestApp_PerClientLimitsUseTheTrustedProxyEntryNotWhatTheClientClaims`.
