# DeepSeek upstream debug - 2026-10-05

Production gateway: 192.168.1.117:18080, v0.1.3. Diagnostic requests used encrypted credentials in memory; tokens, cookies, emails and conversation IDs were not printed. No service/profile restart, secret overwrite or status reset was performed.

## Independent proof of account-level chat mute

Both accounts returned HTTP 200 / code=0 / biz_code=0 on GET /api/v0/users/current. Its biz_data.chat independently contains is_muted=1 and mute_until. The user status field is 0; this report does not infer undocumented status semantics.

Account 1: chat.is_muted=1, chat.mute_until=1791651086.12 (2026-10-10 23:51:26 +07).
Account 2: chat.is_muted=1, chat.mute_until=1791745377.14 (2026-10-12 02:02:57 +07).
The chat object exposes only is_muted and mute_until; no mute reason was returned.

## Request stages

For each account, using both the existing Android headers and a diagnostic web header variant:

- GET /api/v0/users/current: HTTP 200, biz_code 0.
- POST /api/v0/chat_session/create: HTTP 200, biz_code 0; created chat session.
- POST /api/v0/chat/create_pow_challenge: HTTP 200, biz_code 0; returned challenge; local PoW solver proceeded.
- POST /api/v0/chat/completion: HTTP 200, biz_code 5, biz_msg exactly "user is muted", is_muted 1, same mute_until for each account.

The header comparison is HTTP API debugging, not an actual browser UI chat test. The user/current mute state confirms the restriction separately from the completion payload and PoW path. Server-side PoW validation ordering is not observable from these responses.

## Persistence inspection

Both linked profiles exist under data/profiles/deepseek/<profile_id>. Both have Default/Local Storage and Default/Cookies; each cookie database contains three DeepSeek-domain entries. Encrypted credential secrets are readable and authenticate successfully. No cookie or token values were exposed. Profile existence and cookie counts do not by themselves prove future login validity.

## Exact conclusion and limit

Current failure: account-level chat permission is temporarily muted by DeepSeek. Login authentication, session creation and challenge acquisition succeed. The scheduler has two credentials but skips both because their RATE_LIMITED cooldowns are active; the app consequently receives no_available_credential.

Not established: the policy event that caused mute (traffic, content, automation detection, or another upstream decision). Neither inspected response provides a reason. Do not claim a specific cause or promise that changing headers/cookies/restarting will lift the restriction.

Gateway observability should distinguish account-muted cooldown from missing credentials so clients can show the actual reason. This debug run made no production implementation changes.
