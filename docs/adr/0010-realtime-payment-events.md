# 0010. Payment changes reach the app over server-sent events fed by Postgres notifications

Status: accepted
Date: 2026-10-24

## Context
After `POST /v1/payments/topups` the app had to poll `GET /v1/payments/{id}` until the prompt was approved, verified
and credited (seconds to minutes). Paybill credits (ADR 0008) arrive without the app asking at all. Phase 2 slice 5
asks for realtime confirmation. Constraints: the api runs on one free Render instance with the workers in-process
today, but the worker may move to its own service (phase 8), so the process that credits is not necessarily the one
holding the player's connection; there is no message broker besides Postgres and Key Value.

## Decision
- **Transport: server-sent events**, `GET /v1/payments/events`, authenticated like every `/v1` route (Bearer). SSE is
  one-way (all we need), plain HTTP through Render and Cloudflare, and reconnects by itself; WebSocket would add an
  upgrade path, its own auth handshake and a dependency for nothing we send back.
- **Source: a Postgres trigger** on `payments.payments` (migration `00018`) calls `pg_notify('payment_events',
  '<user>:<payment>')` on insert and on a change of status or confirmed amount. Notifications are sent at commit, so
  rolled-back changes are never announced, and every writer (STK verification, paybill settlement, operator actions,
  reversals) is covered without code in each path.
- **Fan-out: `payments.Live`**, one dedicated connection per api instance on `LISTEN`, dispatching to that player's
  subscribers. The payload carries ids only; the stream reads the payment back with the same `Get` as the REST route,
  so a stream never shows more than a GET would, and a stale or forged notification shows nothing new.
- **Delivery is at most once; the stream repairs itself.** It opens with a snapshot (payments still settling or changed
  in the last 15 minutes) so nothing that settled before it opened is missed; after the listener reconnects it sends the
  snapshot again; a subscriber that falls 32 events behind is closed and reconnects. The client keeps the latest state
  per payment id and may see a state twice.
- **Bounds:** 3 streams per player and 2000 per instance, 60 stream opens per player per hour, a heartbeat comment
  every 25 seconds that also re-checks the session, so a stream ends (`expired`) soon after its token expires or is
  revoked. Shutdown ends every stream at once.

## Consequences
- One extra Postgres connection per api instance (the free database allows enough).
- Polling `GET /v1/payments/{id}` keeps working and remains the fallback for clients that cannot hold a stream.
- Later realtime needs (draw results, payouts) can reuse the same pattern with their own channel.
