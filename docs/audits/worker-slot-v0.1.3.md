# Production incident and worker reservation fix - v0.1.3
Date: 2026-10-05 Asia/Saigon. Production: 192.168.1.117:18080.

Two speech synthesis requests failed at 17:38:34 with no worker is available; observability_events records piper:ngochuyennew and 141/142 ms latency. Both GPUs remained connected and processed surrounding requests.

Confirmed code flaw: Dispatch selected a worker before reserving its slot. Another concurrent request could take that slot; hasWorker remained false when a worker had originally been selected, causing a false no-worker rejection instead of a queue wait.

Fix: choose and reserve a slot atomically under the hub lock. Connected workers with exhausted slots retain queue-wait behavior. Missing workers, expired capacity wait and context cancellation return distinct errors. Per-model/GPU concurrency limits remain enforced.

Checks: regression with 80 simultaneous dispatches repeated five times passed. Clean release go test ./... and workerhub go vet passed.

Production v0.1.3 deployed at 17:48:10. /ready reports version 0.1.3; both GPU workers reconnected. Real burst: 40 simultaneous TTS requests, 40 valid WAV, zero failures; latency 0.769-3.074 seconds including queue. GPU0 logged 21 completions; GPU1 logged 19. Gemini 3.8 Flash real two-turn check: HTTP200 at 5.360 and 2.237 seconds, second turn correctly remembered code without resending history. Temporary API key removed.

Remaining diagnosis: four Gemini requests on existing sessions timed out at 90 seconds during 17:04-17:06, plus two client cancellations at 17:07. Current logs do not distinguish session-lock wait, credential-slot wait and upstream-call time. The short current Gemini test does not establish that these exact app requests are fixed. Need the affected app name/error/request timestamp to correlate the reported failure.

DeepSeek accounts remain upstream-muted as recorded in the v0.1.2 audit. /dev/shm exhaustion remains a separate host issue. This release fixes the demonstrated TTS dispatch race, not every possible app failure.

Binary SHA256: 8b2909f2599dd491989c2cd661a50458989ea1a055b45dd0d75ce6d0d45563cf.
Rollback binary: /media/reup/Data_sv4/Sever_provider/sever-provider.pre-0.1.3-20261005-174810.
