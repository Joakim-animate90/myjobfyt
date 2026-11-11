---
name: smoke-auth
description: Run the MyJobFyt authentication flow end to end against the live API — register, read the verification email out of Mailpit, verify, sign in, read and update the profile, rotate the refresh token, and confirm token-theft detection. Use to verify identity changes against the real stack rather than only against unit tests.
---

# Authentication smoke test

Requires the stack and API running — see the `verify-stack` skill.

A feature is not verified because its unit tests pass. This flow exercises the real
Postgres, the real SMTP path and the real cookie handling.

## 1. Register

```bash
API=http://localhost:18080/v1
EMAIL="smoke-$(date +%s)@example.com"
PASS="correct-horse-battery-staple"

curl -s -X POST $API/auth/register -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\",\"full_name\":\"Smoke Test\",\"accepted_terms\":true}"
```

Expect `202` and a deliberately vague message. Now register the **same address again**:
the response must be byte-identical and **no second email may be sent**. Any difference
makes this endpoint an account-enumeration oracle.

## 2. Read the verification link from Mailpit

The link is not in the application log — it goes through real SMTP.

```bash
ID=$(curl -s "http://localhost:18025/api/v1/messages?limit=1" | jq -r '.messages[0].ID')
TOKEN=$(curl -s "http://localhost:18025/api/v1/message/$ID" \
  | jq -r '.Text' | grep -o 'token=[A-Za-z0-9_-]*' | cut -d= -f2)
```

Confirm the inbox holds exactly **one** message for this address, which is what proves
the duplicate registration stayed silent.

## 3. Verify, and confirm the token is single-use

```bash
curl -s -X POST $API/auth/verify-email -H 'Content-Type: application/json' -d "{\"token\":\"$TOKEN\"}"
curl -s -X POST $API/auth/verify-email -H 'Content-Type: application/json' -d "{\"token\":\"$TOKEN\"}"
```

First `200`, second `422` with `token_invalid`.

## 4. Sign in, and check the cookie flags

```bash
curl -s -c jar.txt -X POST $API/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASS\"}" | jq '.data | {token_type, expires_in, user, profile}'
grep myjobfyt_refresh jar.txt
```

The refresh cookie must be `HttpOnly`, scoped to `/v1/auth`. The access token is in the
body; the refresh token must never be.

## 5. Confirm failures are indistinguishable

```bash
curl -s -X POST $API/auth/login -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"wrong-password-entirely\"}"
curl -s -X POST $API/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"nobody-at-all@example.com","password":"correct-horse-battery-staple"}'
```

Both must return the same `401` and the same `invalid_credentials` code.

## 6. Profile round-trip

```bash
AT=<access_token from step 4>
curl -s -X PATCH $API/me/profile -H "Authorization: Bearer $AT" -H 'Content-Type: application/json' \
  -d '{"phone":"+254700123456","city":"Nairobi","country":"KE","headline":"Senior Go Engineer",
       "linkedin_url":"https://linkedin.com/in/example","work_authorisation":"Kenyan citizen",
       "salary_min":90000,"salary_max":130000,"salary_currency":"USD","salary_period":"year"}' \
  | jq '.data.profile | {completeness_pct, form_autofill_ready, has_phone, phone_last_four, missing_fields}'
```

`completeness_pct` should reach 100 and `form_autofill_ready` become `true`. Only
`phone_last_four` may appear — the full number must never be in a response.

Then confirm the rejections: `javascript:alert(1)` as `linkedin_url`, an inverted salary
range, and an unknown field must each give `422` with the offending field named.

## 7. Token rotation and theft detection

This is the most important step.

```bash
OLD=$(grep myjobfyt_refresh jar.txt | awk '{print $NF}')
curl -s -b jar.txt -c jar2.txt -X POST $API/auth/refresh > /dev/null
NEW=$(grep myjobfyt_refresh jar2.txt | awk '{print $NF}')
# the superseded token must be refused AND must kill the family
curl -s -X POST $API/auth/refresh -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$OLD\"}"
# the legitimate newest token must now also be dead
curl -s -X POST $API/auth/refresh -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$NEW\"}"
```

`NEW` must differ from `OLD`, and both of the last two calls must return
`session_revoked`. We cannot tell the thief from the legitimate client, so the whole
family goes.

## 8. Confirm the audit trail and redaction

```bash
docker compose exec -T postgres psql -U myjobfyt -d myjobfyt \
  -c "SELECT action, count(*) FROM audit_logs GROUP BY action ORDER BY action;"
grep -iE "correct-horse-battery|\+254700123456" /tmp/myjobfyt-api.log && echo "LEAK" || echo "clean"
```

`identity.refresh_reuse_detected` must be present. No password or phone number may
appear anywhere in the log.
