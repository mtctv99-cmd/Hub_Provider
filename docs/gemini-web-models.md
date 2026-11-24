# Gemini Web model map (verified 2026-11-13)

Gemini Web addresses models with an opaque 16-hex id (`inner[3]`) plus a numeric
`mode` slot (`inner[79]`). The mapping below was read from the **signed-in**
model picker (`data-mode-id`) and then confirmed with live requests, not guessed.

## Upstream picker (signed-in account)

| Picker label | `data-mode-id` | mode | Chat | Vision | Image |
|---|---|---|---|---|---|
| Trả lời nhanh / 3.5 Flash-Lite | `8c46e95b1a07cecc` | 6 | yes | yes | no |
| Trợ giúp hằng ngày / 4 Hydrogen | `56fdd199312815e2` | 1 | yes | yes | yes |
| Nghiên cứu chi tiết / 3.1 Pro | `e6fa609c3fa255c0` | 3 | yes | yes | no |
| Lập trình & phân tích / 4 Argon | `argon` (→ `95a4c0abcba24364`) | 8 | yes, slow | yes | no |
| Việc phức tạp nhất / 4 Carbon | `upsell_MODE_CATEGORY_ULTRA` (→ `797f3d0293f288ad`) | 3 | yes | yes | no |

The picker also exposes thinking depth (Thấp / Trung bình / Cao) which maps to
`inner[80]` = 1 / 1 / 2. There is no third budget on the web endpoint.

Note: a guest (signed-out) session shows different ids — `cf41b0e0dd7d53e5`
for Flash-Lite — so guest-harvested hexes are not valid for signed-in accounts.

## Gateway roster

| Gateway model id | upstream hex | mode | Notes |
|---|---|---|---|
| `gemini-4-hydrogen` | `56fdd199312815e2` | 1 | default fast tier; chat + vision + image |
| `gemini-3.8-flash`, `gemini-3.6-flash` | `56fdd199312815e2` | 1 | legacy aliases of Hydrogen |
| `gemini-3.5-flash-lite` | `8c46e95b1a07cecc` | 6 | guest allowed; fastest, most accurate |
| `gemini-3.1-pro` | `e6fa609c3fa255c0` | 3 | thinking tier |
| `gemini-4-carbon` | `797f3d0293f288ad` | 3 | deepest tier; slowest |
| `gemini-4-argon` | `95a4c0abcba24364` | 8 | slow (≈3 min); use only for hard work |
| `gemini-image` | `56fdd199312815e2` via `base_model` | 1 | forces the image RPC path, keeps `X-Session-ID` |

Every id above also gets `-low` / `-medium` / `-high` aliases, which only change
`inner[80]`.

## Live measurements (production, 2026-11-13)

| Model | Chat | Image | 5-task accuracy |
|---|---|---|---|
| `gemini-3.5-flash-lite` | 3.7s | — | 1.00 (2.9s TTFT, 7.9 tok/s) |
| `gemini-4-hydrogen` | 8.3s | 15.3s | 1.00 (7.7s TTFT, 4.2 tok/s) |
| `gemini-3.1-pro` | 12.5s | — | 1.00 (6.8s TTFT, 5.3 tok/s) |
| `gemini-4-carbon` | 36.0s | — | 1.00 (6.6s TTFT, 4.9 tok/s) |
| `gemini-4-argon` | 187.5s | — | slow, avoid for chat |
| `gemini-image` | — | 17.9s | — |

## Known upstream quirks

- Image generation works for ids in the upstream image-capable list; the gateway
  gates it per model with `webModels[id].image`.
- Responses carry grounding markers such as `[cite: 1]`; `cleanText` strips them.
- Mode 8 (argon) can take minutes; the gateway timeout is 300s so a stalled
  request releases its lease instead of blocking for 15 minutes.
- Two credentials were `DEGRADED` and two `RATE_LIMITED` during verification;
  those accounts return upstream timeouts until their cookies are refreshed.
- System-prompt adherence is nondeterministic and not guaranteed by any of these
  models. See `docs/chat-api-limits.md` for the measurement method and the
  small-sample caveat; the accuracy column above is answer quality, which is a
  different measurement from instruction following.

## R&D notes (2026-11-13)

Measurements taken with the opt-in harness in
`internal/provider/gemini/live_probe_test.go` (`GEMINI_PROBE_COOKIE_FILE=…`,
optionally `GEMINI_PROBE_DUMP=…`). The harness never runs in CI: it skips when
the env var is missing.

**mode 8 is the problem, not the argon id.** `95a4c0abcba24364` and the string id
`argon` both hang past 150s with mode 8, and both answer in <10s with mode 1:

| probe | mode | result |
|---|---|---|
| hydrogen `56fdd…` | 1 | 6.4s |
| lite `8c46…` | 6 | 3.8s |
| pro `e6fa…` | 3 | 9.5s |
| argon `95a4…` | 8 | timeout |
| argon `95a4…` | 1 | 6.8s |
| argon (string id) | 8 | timeout |
| argon (string id) | 1 | 17.4s |
| carbon `797f…` | 3 | 68s |

Upstream names itself: the response frame carries `inner[39]` (model id) and
`inner[42]` (display label). For `56fdd199312815e2` it reports **"4 Hydrogen"**,
which confirms the picker-derived mapping from the server side.

**Code-interpreter responses used to be dropped.** With thinking level 1
(low/medium) and a multi-step arithmetic prompt, Gemini answers by running Python
first: the text starts with `````python?code_reference…````` and
`````text?code_stdout…````` fences. `cleanText` truncated everything from the
first fence onwards, so those turns returned
`empty_response: parser_failed (raw_len≈61KB)` even though the answer was present.
`stripToolCodeBlocks` now removes only fences tagged `?code_` and keeps the prose;
normal fenced code blocks are untouched. After the fix the same prompt returns
383–400 characters of answer at every thinking level.


## Thinking slot `candidate[37][0][0]`

Gemini's web UI shows a "Show thinking" panel fed by a second, independently
cumulative snapshot inside each `wrb.fr` frame: `inner[4][0][37][0][0]`
(`candidate[37][0][0]`), a plain string. `parseFrame` reads it through
`candidateThoughts`, which length- and type-checks all four levels and returns
`""` on any mismatch — a missing or reshaped slot must never panic the stream
goroutine.

- Non-stream `Chat` sets `ReasoningContent: ExtractResponseThoughts(raw)`; the
  reasoning comes from the frame with the longest answer so text and chain stay
  in the same turn.
- `StreamChat` emits `StreamChunk{ReasoningContent: delta}` on its own branch,
  before the content delta, so a frame that only advances thinking still ships.
  A snapshot that is not an extension of the previous one is adopted silently
  instead of replayed.
- Thoughts are not passed through `cleanText` (they may legitimately contain
  citations, fences or markdown), only `TrimSpace`d.
- Thinking never enters the prompt: `formatPrompt` uses `Message.TextContent()`
  (`internal/provider/provider.go`), i.e. content only.
- Anthropic `thinking` blocks built by `superset_handlers.go` still carry no
  `signature`, same as the pre-existing deepseek behaviour.

**Not live-captured in this repo.** The slot shape comes from the public
protocol docs and sibling implementations, not from a captured frame here. Run
the opt-in harness (`GEMINI_PROBE_COOKIE_FILE=… GEMINI_PROBE_DUMP=…`, then
`TestLiveThinkingSlotProbe`) to confirm before trusting it. Models without a
thinking tier can return an empty chain — that is upstream, not a parser bug.
