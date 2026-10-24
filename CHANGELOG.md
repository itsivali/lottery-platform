# Changelog

## 1.0.0 (2026-10-24)


### Features

* **api:** v1 auth, kyc and responsible-gaming endpoints with openapi contract ([#17](https://github.com/itsivali/lottery-platform/issues/17)) ([b13de6d](https://github.com/itsivali/lottery-platform/commit/b13de6dace9b99c895a1d58af672881cfe21747b))
* **api:** wire v1 into cmd/api with river-backed kyc, managed-postgres migrations and trusted-proxy config ([#19](https://github.com/itsivali/lottery-platform/issues/19)) ([c76aaa8](https://github.com/itsivali/lottery-platform/commit/c76aaa83984189ff51ee1124aa61ef3b2aaead60))
* **db:** phase 1 schema, roles, ledger and wallet invariants, clock ([#12](https://github.com/itsivali/lottery-platform/issues/12)) ([d003bf2](https://github.com/itsivali/lottery-platform/commit/d003bf247bb3514a58a3037994b0e8acd95dba58))
* **draws:** the draw lifecycle from frozen entries to approved, funded results (phase 4 slice 2) ([#33](https://github.com/itsivali/lottery-platform/issues/33)) ([cb2b28d](https://github.com/itsivali/lottery-platform/commit/cb2b28dcddf6d9ace81968023021e80187db786f))
* **draws:** verified drand beacons; phase 4 plan (slice 1) ([#32](https://github.com/itsivali/lottery-platform/issues/32)) ([dbb521d](https://github.com/itsivali/lottery-platform/commit/dbb521d9c0e11f7a15d6b3692b944f373a2e3e67))
* **game:** daily game reconciliation and odds simulation (phase 3 slice 3) ([#31](https://github.com/itsivali/lottery-platform/issues/31)) ([e4174f0](https://github.com/itsivali/lottery-platform/commit/e4174f0f918275f0fff7a3e3da4ee94dcf9d7ba4))
* **game:** paid plays with a provably fair spin (phase 3 slice 2) ([#29](https://github.com/itsivali/lottery-platform/issues/29)) ([4e9363f](https://github.com/itsivali/lottery-platform/commit/4e9363fae49ddb7a9b26c05fa6d2bf3c4d71b827))
* **game:** provably fair spin engine and independent verifier; close phase 2 ([#28](https://github.com/itsivali/lottery-platform/issues/28)) ([81a7ce8](https://github.com/itsivali/lottery-platform/commit/81a7ce856828aae8461366df345d475913b52caf))
* **httpx:** opt-in diagnostic that logs client address headers ([#9](https://github.com/itsivali/lottery-platform/issues/9)) ([f689bc8](https://github.com/itsivali/lottery-platform/commit/f689bc8f43e65075f563bad3e750a0f0d3633293))
* **identity:** otp sign-in, tokens, sessions, devices and sso linking ([#15](https://github.com/itsivali/lottery-platform/issues/15)) ([3d97a85](https://github.com/itsivali/lottery-platform/commit/3d97a8588461af277f145449a1f3f9b097e6d909))
* **identity:** password accounts with phone ownership proved by m-pesa, no sms ([#23](https://github.com/itsivali/lottery-platform/issues/23)) ([e21fb12](https://github.com/itsivali/lottery-platform/commit/e21fb1216aa13bfc7f602044df1fa21372492a1d))
* **kyc:** manual identity review without a registry provider ([#23](https://github.com/itsivali/lottery-platform/issues/23)) ([8e1abd6](https://github.com/itsivali/lottery-platform/commit/8e1abd62f637c6139de97f97992dbe5dc9df82a1))
* **kyc:** national-id verification, responsible-gaming limits and eligibility ([#16](https://github.com/itsivali/lottery-platform/issues/16)) ([6a9a524](https://github.com/itsivali/lottery-platform/commit/6a9a52422cffe552adac807500a9739fb4b08f2d))
* **payments:** buy goods top-ups into a till; shelve the paybill ([#26](https://github.com/itsivali/lottery-platform/issues/26)) ([8f6fd5f](https://github.com/itsivali/lottery-platform/commit/8f6fd5f1a33197cd1dd496426bf9ec09a5492eaf))
* **payments:** daraja client and fault-injecting mock (phase 2 slice 1) ([#26](https://github.com/itsivali/lottery-platform/issues/26)) ([154360c](https://github.com/itsivali/lottery-platform/commit/154360c555a1a01096c964fe6417a204d0ee67d4))
* **payments:** paybill (c2b) payments verified with transaction status (phase 2 slice 3) ([#18](https://github.com/itsivali/lottery-platform/issues/18)) ([6bea730](https://github.com/itsivali/lottery-platform/commit/6bea7309c4a44cc5362998c932a7e020f1887ce8))
* **payments:** realtime top-up confirmation over server-sent events ([#25](https://github.com/itsivali/lottery-platform/issues/25)) ([c477b41](https://github.com/itsivali/lottery-platform/commit/c477b4110e1ee5d97fd3171d147b53410dac24ef))
* **payments:** reversals, per-player debt and daily internal reconciliation (phase 2 slice 4) ([#13](https://github.com/itsivali/lottery-platform/issues/13)) ([0919ad4](https://github.com/itsivali/lottery-platform/commit/0919ad433bea8ebc8dc8008a876a901cfb2646d3))
* **payments:** stk top-up end to end (phase 2 slice 2) ([#27](https://github.com/itsivali/lottery-platform/issues/27)) ([2d75fad](https://github.com/itsivali/lottery-platform/commit/2d75fad483c3623ebed477d919314cbf89aa002f))
* **platform:** idempotency middleware, valkey rate limiter and audit writer ([#14](https://github.com/itsivali/lottery-platform/issues/14)) ([77a3ea3](https://github.com/itsivali/lottery-platform/commit/77a3ea30dc535dbe18adad8af6b9590bc2a8f772))
* **platform:** opentelemetry wiring, erd generator and phase 1 checkpoint ([#18](https://github.com/itsivali/lottery-platform/issues/18)) ([73743fc](https://github.com/itsivali/lottery-platform/commit/73743fcc00230cd044ca00d40374ad0ce9d51e28))
* **wallet:** ledger posting rules, journal store and wallet core ([#13](https://github.com/itsivali/lottery-platform/issues/13)) ([2d1027e](https://github.com/itsivali/lottery-platform/commit/2d1027ef7c6dbf04ecdf5c238e1ec1e25d5cf012))


### Bug Fixes

* **config:** a generated callback token alone does not turn payments on ([#29](https://github.com/itsivali/lottery-platform/issues/29)) ([36dbef6](https://github.com/itsivali/lottery-platform/commit/36dbef6d1572ca4e3b213c5e868524edd47e8e2b))
* **deploy:** read the client address from CF-Connecting-IP on Render ([#10](https://github.com/itsivali/lottery-platform/issues/10)) ([64e13f5](https://github.com/itsivali/lottery-platform/commit/64e13f5c571122a691826cf681fc0c792b342f83))
* make Dockerfile build the api by default and add Render blueprint ([#11](https://github.com/itsivali/lottery-platform/issues/11)) ([90cd704](https://github.com/itsivali/lottery-platform/commit/90cd7048b4c0dd5be577dfb7acfa7e88b613d40b))
* **payments:** accept the sandbox's zero-padded success code; use shortcode 600992 ([#19](https://github.com/itsivali/lottery-platform/issues/19)) ([64da7b1](https://github.com/itsivali/lottery-platform/commit/64da7b1a21faa0259e5420a723c6a1b557c2aeda))
* **payments:** pace STK status queries so Daraja does not throttle them ([#30](https://github.com/itsivali/lottery-platform/issues/30)) ([75eb06a](https://github.com/itsivali/lottery-platform/commit/75eb06a07257bc314218f454c1771ed9bbc2f217))
* **payments:** tie top-ups to the paybill and say why Daraja refuses a request ([#27](https://github.com/itsivali/lottery-platform/issues/27)) ([ead450b](https://github.com/itsivali/lottery-platform/commit/ead450b1b8996f74c1a6a76f9ccc2f3cc55b4aba))
