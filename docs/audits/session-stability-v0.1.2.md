# Session stability audit - v0.1.2

Production: reup@192.168.1.117, /media/reup/Data_sv4/Sever_provider, API port 18080.
Audit and verification date: 2026-10-05 (Asia/Saigon).

## Confirmed causes and corrections

- Session bindings previously lived in RAM. Restart, idle sweep and the turn limit could lose chat continuity. Production now uses chat_session_bindings in SQLite. Persistent mappings do not expire at 30 minutes or reset at 25 turns. Six existing live bindings were exported before deployment.
- Returning chats previously tried other credentials while carrying the original account's upstream IDs. Returning chats now stay on their owning account/provider; unavailable owners produce an error while retaining the mapping. Image calls no longer modify chat bindings.
- Concurrent turns could reuse the same upstream parent. A context-aware per-session lock serializes chat and streaming requests and cleans up idle locks.
- Lease TTL did not bound the actual upstream call. Chat and streamed chat now have actual request deadlines, including queue wait.
- Stream completion could be reported before saving the mapping. Mapping commit now precedes terminal success. Abrupt stream closure and failed database commits return errors.
- DeepSeek HTTP 200 returned application/json with biz_code=5, is_muted=1 and mute_until for both accounts. Previously the parser ignored the response and returned an empty successful answer. These responses now produce a typed rate-limit failure. Account cooldown uses the actual upstream expiry, with a retry permitted after expiry.
- New DeepSeek conversations no longer reuse a credential-level chat_session_id. Continuation sends the stored parent_message_id, and parser/state support preserves response message IDs.
- Empty/truncated DeepSeek streams fail instead of reporting success. Parser sends respect cancellation.
- Cancelled non-streaming chat failures now use an independent bounded context for the audit write.
- Gemini generated-image extraction no longer treats URLs in prose as generated images and rejects ambiguous image attachments.

## Verification

- Clean release checkout: go test ./... PASS; targeted go vet PASS.
- Regression coverage: persistent restart/idle/30 turns, owner mismatch, failed DB commit, cancelled session-lock wait and lock cleanup, no account failover for bound chat, stream commit ordering, abrupt close, DeepSeek mute expiry, parent-message payload and message-ID capture, empty/truncated streams.
- Production v0.1.2: /ready reports ready, 31 configured models. API and both GPU TTS user services active.
- Real Gemini 3.8 Flash: first turn 2.898 s; second turn after server restart 3.288 s, remembered the first turn's random code without resending history. Credential and upstream conversation IDs unchanged. Streaming continuation 2.522 s, correct remembered code, DONE, no stream errors.
- Real DeepSeek: rejected with HTTP 502 rather than false HTTP 200; both credentials became RATE_LIMITED with upstream cooldowns: 2026-10-10 23:51:26 +07 and 2026-10-12 02:02:57 +07. Full real DeepSeek continuation cannot be proven while upstream has muted both accounts.
- TTS short Vietnamese sample: HTTP 200, RIFF/WAV 118828 bytes, 0.288 s. This is one short-request measurement, not a throughput benchmark.
- Three concurrent 82-character TTS samples returned valid WAV in 0.350 / 0.355 / 0.323 s. GPU0 logged two completions (299.4 / 303.3 ms inference), GPU1 logged one (272.0 ms). Both GPU workers therefore executed actual requests.
- Post-deploy log window: zero no-available-credential messages, zero refresh-failed messages, zero no-worker-available messages, zero panics. One expected account-muted error from the DeepSeek verification. Observation window is short; no long-duration soak claim.
- Live storage: root 87% used (59 GB available), Data_sv4 50% used (447 GB available), /dev/shm 100% used (12 KB available). Shared-memory exhaustion remains a separate host issue; no files were deleted in this session.
- Temporary verification API key deleted. Six pre-existing session mappings retained in SQLite.

## Operational limits

X-Session-ID must be stable and unique for each intended conversation. Requests without it do not select an old chat. All holders of an API key should use distinct opaque IDs for unrelated chats; the header is not an authentication credential.
Persistent mappings cannot prevent upstream conversation deletion, account logout/expiry, provider-side limits, machine/disk failure, or recover mappings already swept by the old version. Back up orchestrator.db and browser profiles securely; neither cookies nor secrets are included in this release.
This audit verifies the session path and Gemini 3.8 Flash live, not every advertised provider/model. Colab remains outside the active test scope.

## Rollback

Binary backup: /media/reup/Data_sv4/Sever_provider/sever-provider.pre-0.1.2-20261005-162902.
The new SQLite table is additive. Returning to v0.1.1 loses persistent-session behavior but does not require dropping the table.
Binary SHA-256: 8d68b5bda98a82e0cdf56d5723593f00747d3d34857ca4440f0f5972ba9499f2.
