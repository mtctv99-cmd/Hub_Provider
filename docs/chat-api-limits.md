# Tham số chat: cái gì thật sự có tác dụng

Tài liệu này mô tả hành vi **thật** của gateway với các tham số sampling/limit trên
`/v1/chat/completions` và `/v1/messages`. Các web upstream (Gemini, DeepSeek,
Copilot, Meta, TikTok) là API reverse-engineered: chúng không có slot cho
temperature/top_p/seed/penalty, nên gateway **không thể** forward các giá trị đó.

Đo trên gateway `0.1.51` (`curl /ready` → `{"status":"ready","version":"0.1.51"}`),
ngày 2026-11-13. Hành vi có thể lệch khi adapter được sửa.

## Quy tắc chung

`decodeJSON` (`internal/gateway/handlers.go:1351`) dùng `json.NewDecoder(...).Decode`
thường, **không** `DisallowUnknownFields`. Vì vậy field không có trong struct request
bị **bỏ im lặng**: không lỗi 400, không echo lại, client không có tín hiệu nào ngoài
việc kết quả không đổi. Muốn biết field nào được nhận, đọc struct:
`ChatCompletionRequest` (`internal/gateway/types.go:10`),
`AnthropicMessageRequest` (`internal/gateway/types.go:65`).

Không có warning header: gateway giữ hợp đồng OpenAI/Anthropic để client
schema-strict không vỡ.

## Bảng tham số

| Param | `/v1/chat/completions` | `/v1/messages` | Hành vi thật | Nguồn |
|---|---|---|---|---|
| `temperature` | nhận (`*float32`) | không có field → bỏ | Không range-check (3.5 hay -9 vẫn 200). Chỉ worker Colab/vLLM dùng; adapter web bỏ qua | `types.go:18`, `handlers.go:392`, `workerhub/chat.go:94`, `colab/colab.go:85,123` |
| `top_p` | không có field → bỏ | không có field → bỏ | Không tác dụng với mọi provider; adapter Colab gửi thẳng `"top_p": null`, workerhub thì không gửi | `colab/colab.go:85,123`, `workerhub/chat.go:94` |
| `top_k` | bỏ | bỏ | Chỉ có nghĩa với Anthropic native, gateway không xử lý | — |
| `seed` | bỏ | bỏ | Không có tính tất định. Muốn nối tiếp ngữ cảnh dùng `X-Session-ID` (chỉ provider web đi qua Dispatcher) | `provider/provider.go:263` |
| `presence_penalty` | bỏ | bỏ | Không tác dụng | — |
| `frequency_penalty` | bỏ | bỏ | Không tác dụng | — |
| `logprobs:true`, `top_logprobs>0`, `parallel_tool_calls:false` | 400 `unsupported_parameter` | bỏ | Gateway không forward được nên từ chối thay vì trả lời sai; `logprobs:false`, `top_logprobs:0`, `parallel_tool_calls:true` vẫn được chấp nhận | `handlers.go` `unsupportedChatParameter` |
| `logit_bias`, `user` | bỏ | bỏ | Không tác dụng | — |
| `max_completion_tokens` | bỏ | bỏ | Không phải alias của `max_tokens` | — |
| `response_format` | nhận `text`/`json_object`/`json_schema` | bỏ | Prompt-level + kiểm tra tối thiểu, không phải schema validator; xem `docs/response-format.md` | `internal/gateway/structured_output.go` |
| `max_tokens` | nhận (int) | nhận (int) | Gateway tự cắt, xem dưới | `handlers.go:480`, `650`; `superset_handlers.go:90,199` |
| `stop` | nhận **array** (`[]string`) | — | Gateway tự cắt, xem dưới | `types.go:20` |
| `stop_sequences` | — | nhận (`[]string`) | Cắt như `stop`; `stop_sequence` trong response luôn `null` | `types.go:73`; `superset_handlers.go:160` |
| `n` | `>1` → 400 `unsupported_parameter`; `0` hoặc âm coi như 1 | không có field | Web provider trả 1 lượt/request | `handlers.go:351` |
| `reasoning_effort` | nhận | nhận | Chỉ adapter Gemini Web dùng, xem dưới | `payload.go:185` |
| `tools` / `tool_choice` | nhận | nhận | Chỉ gemini_web/copilot_ws/deepseek_web dựng tool prompt; `meta_web` và worker Colab bỏ qua | `provider/tools.go:120`, `meta/client.go:66`, `workerhub/chat.go:94` |

## `max_tokens`: cắt phía gateway, không phải tokenizer

- Giới hạn là `max_tokens * 4` **rune** trên `content` (`handlers.go:799-825`,
  enforce ở `handlers.go:867-877`, cùng tỉ lệ với `provider.EstimateTokens`).
  `max_tokens <= 0` tắt cap.
- Chỉ áp cho `Content`. `reasoning_content` và `tool_calls` **không** bị tính/cắt.
- Non-stream: cắt xong, gateway **chỉ** set `finish_reason = "length"` khi truncated
  (`handlers.go:481-483`). Khi stop sequence khớp, `finish_reason` giữ nguyên giá
  trị adapter trả về (thường là `"stop"`), gateway không ép lại.
- Stream: khi limiter đã dừng, các chunk sau bị bỏ (`handlers.go:699`, `744`) và
  `finish_reason` bị ép thành `stop`/`length` **đè** `tool_calls` (`handlers.go:722-729`).
  Nếu truncation và tool call đến cùng lúc, tool call có thể bị mất.
- `/v1/messages`: cùng limiter; `stop_reason` ưu tiên `max_tokens` > `stop_sequence`
  > `tool_use` > `end_turn` (`superset_handlers.go:91-99`, `268-292`).
- Nhánh worker Colab/vLLM (`handlers.go:399-431`, `internal/gateway/colab_chat.go:39`)
  **không** đi qua limiter: `max_tokens` được forward nguyên, việc cắt do worker
  quyết định (code worker nằm ngoài repo → chưa xác nhận).
- Stream ảnh (`image_generation`, `handlers.go:553`) không dùng limiter.
- Đây là xấp xỉ, không phải BPE tokenizer: văn bản nhiều token/rune sẽ ngắn hơn
  kỳ vọng khi quy ra token thật.

## `stop` / `stop_sequences`: cắt substring chính xác

- OpenAI route chỉ nhận array. Gửi `"stop": "charlie"` (string) → 400 `invalid_json`
  vì decode struct lỗi kiểu; OpenAI chấp nhận cả hai, gateway thì không.
- Khớp **substring chính xác, phân biệt hoa-thường** trên `content`, phần khớp và
  sau đó bị bỏ; không echo lại chuỗi stop trong response.
- Worker Colab/vLLM không nhận `stop` (`workerhub/chat.go:94`) → với các model này
  stop **không có tác dụng** ở cả hai đầu.
- Anthropic route: `stop_reason = "stop_sequence"` khi khớp, nhưng
  `stop_sequence` luôn `null` (`superset_handlers.go:160`) — SDK đọc giá trị này
  sẽ thấy `None`. `usage` của route này là `input_tokens`/`output_tokens`
  (`superset_handlers.go:160-164`, `218`) và cũng là ước lượng, không phải
  tokenizer của upstream.

## `reasoning_effort`: chỉ Gemini Web

- Map trong `internal/provider/gemini/payload.go:185`: `""|minimal|low|medium` → 1
  (standard thinking), `high` → 2 (extended), giá trị khác → lỗi
  `unsupported Gemini reasoning_effort`.
- Alias `-low/-medium/-high` chỉ sinh cho model `gemini_web` có capability
  `reasoning` (`internal/scheduler/registry.go:77-95`). Model `image_mode` được
  register trực tiếp, không alias (`cmd/server/main.go:66-71`): gửi `reasoning_effort`
  khác rỗng tới các model này → 400 `reasoning_effort_conflict`
  (`handlers.go:245-259`), không phải bị bỏ qua.
- `deepseek_web`, `copilot_ws`, `meta_web`, `tiktok_api`, worker Colab không đọc
  field này → bỏ qua, không validate.

## Provider web vs worker Colab

Worker chỉ nhận 4 field (`internal/workerhub/chat.go:94`):
`messages`, `stream`, `temperature`, `max_tokens`. Không có `tools`,
`tool_choice`, `session_id`, `reasoning_effort`, `stop`, `image_generation`.
Hệ quả:

- Tools/`X-Session-ID`/`reasoning_effort` trên model `google_colab`/`vllm` bị bỏ.
- `stop` không được forward, và gateway cũng không cắt → không tác dụng.
- Đường worker cũng bypass `Dispatcher` nên không qua session manager.

## Cách tự đo

```bash
curl -s http://127.0.0.1:18080/ready
# max_tokens: nhỏ hơn nhiều so với độ dài câu trả lời
curl -s http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"<model>","messages":[{"role":"user","content":"Count from 1 to 200"}],"max_tokens":4}'
# stop: model phải in đúng câu chứa chuỗi stop
curl -s http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"<model>","messages":[{"role":"user","content":"Reply exactly: alpha BRAVO charlie delta"}],"stop":["charlie"]}'
```

Số đo trong lần điều tra trước (chưa chạy lại trong phiên này, single-sample, nội
dung model phi tất định nên không phải benchmark): `max_tokens=4` trả 16 rune với
`finish_reason=length`; `stop=["charlie"]` trả `alpha BRAVO ` với
`finish_reason=stop`; `stop:"charlie"` trả 400 `invalid_json`;
`max_completion_tokens=4` trả đủ nội dung với `finish_reason=stop`.

`scripts/chat_feature_probe.py` **không** dùng làm bằng chứng: file còn untracked
và hardcode API key. Nếu muốn dùng, truyền key qua `--key`/env, siết assert
(`len(content) <= max_tokens*4`, kiểm `finish_reason`), rồi commit.

## System prompt adherence (prod 0.1.51, 2026-11-13)

Không phải param, nhưng cùng loại giới hạn: system message không được upstream
bảo đảm tuân thủ.

**Cách đo.** `POST /v1/chat/completions`, `messages` = system
`"Always answer with the single word BANANA."` + user `"What is 2+2?"`, key
master. Mỗi mẫu phải ghi `http_status` và body; chỉ tính là "đạt" khi status 200
và `choices[0].message.content` khớp `banana` (không phân biệt hoa/thường).
Response lỗi hoặc content rỗng ghi riêng là `error/empty`, **không** tính là vi
phạm system.

**Số liệu hiện có (mẫu nhỏ, chưa đủ để xếp hạng).** Một phiên 3 lần/model cho
`gemini-3.5-flash-lite` 2/3, `gemini-4-hydrogen` 1/3, `gemini-3.1-pro` 3/3; lần
đo 6 lần/model cho lite 4/6, hydrogen 4/6, pro 5/6 (1 response 0 byte). Dao động
1/3–3/3 giữa các batch, chênh lệch 4/6 vs 5/6 là nhiễu với n=6 — **không** phân
biệt được lite kém hơn hydrogen. Muốn khuyến nghị theo model thì chạy ≥20
lần/model và ghi kèm status từng mẫu.

**Auth path ảnh hưởng kết quả.** `gemini-3.5-flash-lite` có `guest: true` trong
`config/models.yaml`, nhưng guest chỉ được dùng khi không còn credential
(`internal/scheduler/dispatch.go:123`); có credential thì request đi đường
signed-in (hex `8c46e95b1a07cecc`, `docs/gemini-web-models.md:29`). Guest dùng hex
khác `cf41b0e0dd7d53e5` (`docs/gemini-web-models.md:20`), nên số đo trên đường
signed-in không suy rộng cho guest.

**Vì sao không sửa gateway.** System message được đưa vào prompt dạng plain prose,
không nhãn (`internal/provider/tools.go:174-175`), dùng chung cho `Chat` và
`StreamChat` (`internal/provider/gemini/rpc.go:27`, `:146`); Anthropic route chỉ
thêm system thành message đầu (`internal/gateway/superset_handlers.go:69`). Không
có nhánh nào bỏ system. Thêm nhãn `SYSTEM INSTRUCTION (must follow):` không cải
thiện (n=3: lite 0/3, hydrogen 0/3) và `FormatPromptWithTools` đang được pin bởi
`internal/provider/tools_test.go:91` (plain prose được xác nhận live), nên giữ
nguyên formatter.

**Đừng nhầm với accuracy.** Bảng "5-task accuracy 1.00" trong
`docs/gemini-web-models.md:40-45` là độ đúng của câu trả lời, không phải mức tuân
thủ system instruction. Hai phép đo khác nhau.

**Phạm vi.** Chỉ đo non-stream; stream dùng cùng formatter nhưng chưa đo.
`scripts/chat_feature_probe.py` chỉ ghi PASS/PARTIAL (PARTIAL gồm cả lỗi HTTP),
không phải test hồi quy cho adherence.

## Vision: ảnh gửi bằng URL http(s)

`image_url` dạng `data:` vẫn được xử lý cục bộ. Với URL `http(s)` (Gemini Web
tải ảnh rồi upload lên Scotty), gateway áp guard SSRF dùng chung với Copilot
(`internal/provider/imagefetch.go`):

- chỉ `https` (Gemini chấp nhận cả `http` khi host công khai), không
  userinfo/fragment/IP literal/host không có dấu chấm/`.localhost`, port phải là
  mặc định (443, hoặc 80/443 với http);
- DNS được resolve trước khi dial và mọi IP phải là public (chặn loopback,
  private, link-local, CGNAT, documentation ranges);
- `HTTP_PROXY` bị bỏ qua để proxy không vòng qua được guard;
- redirect tối đa 4 hop, mỗi hop validate lại, cấm tụt `https` → `http`;
- timeout 15s, body tối đa 16MB, response phải là `image/*` (header hoặc sniff).

URL vi phạm → 400 `invalid_image_url` (message "image URL is invalid or not
publicly reachable"), không phải 502. Hạ tầng nội bộ không truy cập được qua
`image_url`; nếu cần ảnh nội bộ thì gửi `data:` URI.

## Ranh giới system prompt và prompt injection

Provider web chỉ nhận một chuỗi prompt, nên `system` và nội dung người dùng nằm
chung một trường. Gateway cố ý không thêm nhãn `[System Instruction]:` hay
`[User]:` vì Gemini bỏ qua chỉ thị khi gặp nhãn dạng này (đo live: adherence
giảm từ 3/3 xuống 0-2/3). Hệ quả: **nhãn không phải hàng rào bảo mật** — người
dùng vẫn có thể chèn câu lệnh đè lên chỉ thị của gateway.

Hàng rào thật nằm ở phía sau:

- `response_format` được kiểm tra cấu trúc + retry một lần (`structured_output.go`).
- Tool call được parse theo khối `tool_call`; khối không hợp lệ bị bỏ, không thực thi.
- Kết quả trả về không được dùng làm lệnh hệ thống ở tầng gateway.

Nếu cần cách ly mạnh hơn, phải dùng provider có system instruction native
(API chính thức), không phải Gemini web.
