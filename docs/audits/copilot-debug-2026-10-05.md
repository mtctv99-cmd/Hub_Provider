# Copilot production debug - 2026-10-05

Production v0.1.3, 192.168.1.117:18080. Read-only diagnostic scope: inspect credential/runtime metadata and send synthetic test chats. No service restart, production adapter modification, credential reset, profile overwrite or token values in output.

## Live authentication and fresh chat

All six encrypted credentials are readable, token URLs pass structural/expiry checks and real fresh-chat requests return nonempty OK. Account-order latency: 5.505 / 3.913 / 4.985 / 4.723 / 6.571 / 5.125 seconds. All return conversation ID, chat session ID and invocation=1. Earlier production requests were also recently recorded as successful, but event success alone cannot establish nonempty continuation.

## Confirmed continuation failure

Public API gpt-5.6 with a temporary chat-scoped API key and the same opaque X-Session-ID:
- Turn 1: HTTP200, nonempty, 8.554 s, binding invocation 1.
- Turn 2: HTTP200, EMPTY content, 1.000 s, same credential/conversation, invocation 2. Does not remember first-turn code.
- Turn 3 streamed: HTTP200, EMPTY content, DONE, no surfaced error, 1.075 s.
Temporary key deleted; temporary persistent binding deleted.

Raw WebSocket reproduction using the actual production invocation template:
- HTTP/WebSocket protocol handshake succeeds on both turns.
- First terminal type=2: item.result.value=Success.
- Second terminal type=2: item.result.value=InvalidRequest; message="Sorry, I wasn't able to respond to that. Is there something else I can help with?".
- Returned conversation ID matches the requested ID.
- Upstream provides conversationTransferToken and turnState on the successful first turn. Production adapter does not preserve them.
- Diagnostic inclusion of both returned fields in the next request still returns InvalidRequest. Resetting invocationId to zero and setting isStartOfSession=true with the same conversation also fails. Therefore the exact continuation protocol field/sequence mismatch is not established; missing state fields alone are NOT proven as the cause.

## Confirmed source defects

internal/provider/copilot/copilot.go generate checks top-level event.Error but ignores terminal item.result.value. A terminal InvalidRequest therefore returns empty string with nil error. Chat/StreamChat mark it successful, and scheduler advances the binding invocation counter.
The adapter also accepts partial text as success after WebSocket read failure, and does not connect context cancellation to the active WebSocket read. These two are source findings; fault injection was not performed in this audit.

## Token lifecycle gap

System copilot-refresh.timer runs every 12 hours. Last trigger 2026-10-05 17:57:18 +07; service exited successfully 17:58:36. Next scheduled run 2026-10-06 05:57:18 +07.
Observed token expiries, account order: 2026-10-05 21:22:42 / 21:23:53 / 21:17:59 / 21:16:23 / 21:27:32 / 20:38:52 (+07). All six expire before the next scheduled refresh. No assertion that current short tests prove continuous overnight availability.
All six copilot_ws credential source_ref fields are empty; eight profile-directory entries exist. Identity-to-profile recovery links are not established by these counts.

## Conclusion

Fresh chats currently work on all six accounts. Existing conversation continuation is demonstrably broken and incorrectly advertised as success. Timed token refresh is misaligned with observed expiry. A robust repair needs correct upstream continuation semantics, error propagation/empty-response rejection, context-bound socket lifecycle and expiry-aware refresh/profile ownership. This debug run does not claim these production defects have been repaired.
