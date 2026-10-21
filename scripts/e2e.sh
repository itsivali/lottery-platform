#!/usr/bin/env bash
# E2E: bring up the stack, check the probes, then sign in and verify identity through the real API and worker.
# Playwright journeys for the clients arrive with the clients.
set -euo pipefail
docker compose up -d --build --wait
trap 'docker compose down -v' EXIT
curl -fsS localhost:8080/healthz >/dev/null
curl -fsS localhost:8080/readyz >/dev/null
for p in 8090 8091 8092 8093; do curl -fsS "localhost:$p/healthz" >/dev/null; done

post() { # post <path> <idempotency-key> <json> [token]
  curl -sS -o /tmp/e2e.body -w '%{http_code}' -X POST "localhost:8080$1" -H "Idempotency-Key: $2" \
    -H 'Content-Type: application/json' ${4:+-H "Authorization: Bearer $4"} -d "$3"
}

[ "$(post /v1/auth/otp/request e2e-1 '{"msisdn":"0712345678"}')" = 204 ] || { echo "otp request failed"; cat /tmp/e2e.body; exit 1; }
code=""
for _ in $(seq 1 20); do
  code=$(docker compose logs sms-mock 2>&1 | grep -o 'Your verification code is [0-9]\{6\}' | tail -1 | grep -o '[0-9]\{6\}' || true)
  [ -n "$code" ] && break
  sleep 0.5
done
[ -n "$code" ] || { echo "no OTP reached the SMS mock"; exit 1; }
[ "$(post /v1/auth/otp/verify e2e-2 "{\"msisdn\":\"0712345678\",\"code\":\"$code\",\"device\":{\"fingerprint\":\"e2e-device\",\"platform\":\"android\"}}")" = 200 ] \
  || { echo "otp verify failed"; cat /tmp/e2e.body; exit 1; }
token=$(jq -r .access_token /tmp/e2e.body)
[ "$(post /v1/kyc e2e-3 '{"national_id":"12345678","full_name":"Jane Wanjiru Kamau","date_of_birth":"1990-05-17"}' "$token")" = 202 ] \
  || { echo "kyc submit failed"; cat /tmp/e2e.body; exit 1; }
for _ in $(seq 1 60); do
  status=$(curl -fsS localhost:8080/v1/kyc -H "Authorization: Bearer $token" | jq -r .status)
  [ "$status" = verified ] && break
  sleep 0.5
done
[ "$status" = verified ] || { echo "KYC never verified (status: $status)"; docker compose logs api | tail -20; exit 1; }
echo "e2e smoke ok"
