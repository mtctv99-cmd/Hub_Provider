import sqlite3, json, os, uuid
from urllib.parse import urlparse, parse_qs, urlencode, urlunparse
import websocket
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

MASTER_KEY = bytes.fromhex('0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef')
aesgcm = AESGCM(MASTER_KEY)
conn = sqlite3.connect('/media/reup/Data_sv4/Sever_provider/data/orchestrator.db')
row = conn.execute("SELECT encrypted_payload, nonce FROM credential_secrets WHERE credential_id LIKE '%copilot%' LIMIT 1").fetchone()
token_url = json.loads(aesgcm.decrypt(row[1], row[0], None))['token_url']

with open('/media/reup/Data_sv4/Sever_provider/data/copilot/ws_template.json') as f:
    tpl = json.load(f)

req_id = str(uuid.uuid4())
chat_id = uuid.uuid4().hex[:16]
conv_id = str(uuid.uuid4())

u = urlparse(token_url)
q = parse_qs(u.query)
q['chatsessionid'] = [chat_id]
q['XRoutingParameterSessionKey'] = [chat_id]
q['clientrequestid'] = [chat_id]
q['ConversationId'] = [conv_id]
ws_url = urlunparse((u.scheme, u.netloc, u.path, u.params, urlencode(q, doseq=True), u.fragment))

arg0 = tpl['arguments'][0]
arg0['clientCorrelationId'] = req_id
arg0['requestId'] = req_id
arg0['sessionId'] = chat_id
arg0['conversationId'] = conv_id
arg0['tone'] = 'Gpt_5_6_Chat'
arg0['isStartOfSession'] = True

test_img = "https://raw.githubusercontent.com/golang/go/master/doc/gopher/fiveyears.jpg"
arg0['message']['text'] = "Ảnh này có con vật gì hoặc hình vẽ gì? Trả lời thật ngắn gọn."
arg0['message']['imageUrl'] = test_img
arg0['message']['originalImageUrl'] = test_img
arg0['message']['requestId'] = req_id

headers = [
    "Origin: https://m365.cloud.microsoft",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/153 Safari/537.36"
]

ws = websocket.create_connection(ws_url, header=headers)
ws.send('{"protocol":"json","version":1}\x1e')
ack = ws.recv()
print("Handshake ack:", repr(ack))

payload_str = json.dumps(tpl) + '\x1e'
ws.send(payload_str)

while True:
    try:
        frame = ws.recv()
        for raw in frame.split('\x1e'):
            raw = raw.strip()
            if not raw:
                continue
            msg = json.loads(raw)
            msg_type = msg.get('type')
            print(f"[TYPE {msg_type}] keys: {list(msg.keys())}")
            if msg_type == 1:
                item = msg.get('arguments', [{}])[0]
                if 'messages' in item:
                    for m in item['messages']:
                        print("  BOT MSG:", m.get('author'), m.get('text'))
            elif msg_type == 2:
                print("  ITEM RESULT:", msg.get('item', {}).get('result'))
                break
            elif msg_type == 3:
                print("  TYPE 3:", msg)
                break
        if msg.get('type') in (2, 3):
            break
    except Exception as e:
        print("WS Err:", e)
        break

ws.close()
