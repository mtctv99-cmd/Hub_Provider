# `response_format` trên gateway

Phạm vi: chỉ `/v1/chat/completions`. `/v1/messages` (Anthropic) không có khái
niệm này và không bị ảnh hưởng.

Các upstream web (Gemini, DeepSeek, Copilot, Meta, TikTok) không có structured
output native, nên gateway làm hai việc: chèn chỉ thị JSON vào prompt và tự
kiểm tra kết quả. Đây **không** phải JSON Schema validator.

## Giá trị được nhận

| `type` | Hành vi |
|---|---|
| thiếu, `""`, `"text"` | Không xử lý gì thêm, giữ hành vi cũ |
| `"json_object"` | Chèn chỉ thị "chỉ trả một JSON value", yêu cầu kết quả là JSON object |
| `"json_schema"` | Như trên + chèn schema (compact) vào prompt, yêu cầu đúng root kind và đủ `required` top-level |
| khác | 400 `invalid_response_format` |

Với `json_schema`:

- `json_schema.schema` bắt buộc, phải là JSON **object**, tối đa 8192 byte
  (vượt → 400 `unsupported_option`).
- `name` và `strict` được chấp nhận nhưng không có tác dụng.
- Root kind lấy từ `schema.type`:
  - `"object"` → kết quả phải là JSON object;
  - `"array"` → phải là JSON array;
  - mảng type như `["object","null"]` → object (hoặc array nếu chỉ có array);
  - không có `type`, có `$ref`/`oneOf`/union lạ → chấp nhận mọi JSON value.

## Kiểm tra thật sự làm gì

1. Trim khoảng trắng; nếu bọc ```` ```json ... ``` ```` thì bỏ fence.
2. `json.Unmarshal` phải parse được thành một JSON value (không phải `null`).
3. Đối chiếu root kind như bảng trên.
4. Với root object của `json_schema`: kiểm tra **top-level** `required` — mỗi
   tên trong `required` (mảng string hợp lệ) phải có mặt trong object; thiếu →
   lỗi `reply is missing required field "x"`. Giá trị `null` vẫn tính là có
   mặt (không kiểm tra type).
5. **Không** kiểm tra `properties`, type của từng field, `required` lồng nhau,
   `additionalProperties`, `$ref`, `oneOf`/`anyOf`, hay schema lồng nhau. Client
   cần validate lại nếu những ràng buộc đó là bắt buộc.

Khi output sai: gateway gọi upstream **một lần nữa** (retry) với reply cũ
(cắt ~2000 rune) + câu "Your previous reply was rejected: …". Nếu vẫn sai →
502 `provider_error` / `invalid_response_format`. Không echo nội dung reply
vào response lỗi.

Điều kiện retry:

- không có header `X-Session-ID` (session sticky đã bind upstream state; retry
  trên cùng phiên sẽ đẩy thêm một lượt rác vào conversation);
- output không bị cắt bởi `max_tokens`/`stop` (bị cắt → 502 luôn, vì retry sẽ
  bị cắt y hệt);
- request chưa bị client ngắt.

## Giới hạn / không áp dụng

- **Streaming** (`stream:true`): chỉ chèn chỉ thị, **không** kiểm tra — muốn
  enforce phải buffer toàn bộ SSE.
- **Tools**: `response_format` kết hợp với `tools` bị từ chối 400
  `unsupported_option`, vì prompt tool (`FormatPromptWithTools`) và chỉ thị
  "chỉ trả JSON" xung đột nhau. Nếu model tự trả `tool_calls` thì bỏ qua bước
  validate.
- **Worker** (`google_colab`/`vllm`) và **image generation**: `response_format`
  bị bỏ qua.
- Chỉ là prompt-level: model vẫn có thể trả JSON trong `reasoning_content` với
  `content` rỗng — trường hợp đó tính là invalid.
- Retry khiến upstream chạy 2 lượt nhưng usage/AITokens chỉ ghi nhận 1 lượt.
- Không có validator đầy đủ; `required` chỉ được kiểm ở top-level. Nếu cần
  enforce `required` lồng nhau/properties thì phải thêm thư viện JSON Schema và
  thay `validateStructuredOutput`
  (`internal/gateway/structured_output.go`).
