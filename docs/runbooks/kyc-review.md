# Runbook: manual KYC review

Identities are checked by staff (ADR 0009). A player submits national ID number, full name and date of birth; the
account then waits in `manual_review` and cannot top up until a reviewer decides. Under-18s and IDs already verified
on another account are refused before they reach the queue.

## List what is waiting

Listing decrypts personal data, so it needs the api's configuration (copy the api's environment from the Render
dashboard into your shell; never commit it or save it in a file in the repository):

```sh
go run ./cmd/lotteryctl kyc pending -operator <your name>
```

Every submission shown is audited as `kyc.unmask` under `operator:<your name>`. Show it only when you are about to
review it.

## Decide

Confirm the identity by the operator's procedure (owner to define: for example a photo of the ID card sent by the
player, checked against the submitted number, name and date of birth). Then:

```sh
go run ./cmd/lotteryctl kyc approve -user <user id> -reason "<how it was checked>" -operator <your name>
go run ./cmd/lotteryctl kyc reject  -user <user id> -reason "<why>" -operator <your name>
```

Approve and reject need no keys. A reason is required and is stored with the decision. A rejected player may submit
again. Approval fails if the national ID was meanwhile verified for another account: reject instead.
