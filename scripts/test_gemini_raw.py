import sqlite3, json, time, re, urllib.request, urllib.parse, hashlib
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

MASTER_KEY = bytes.fromhex('0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef')
aesgcm = AESGCM(MASTER_KEY)

conn = sqlite3.connect('/media/reup/Data_sv4/Sever_provider/data/orchestrator.db')
rows = conn.execute("SELECT credential_id, encrypted_payload, nonce FROM credential_secrets WHERE credential_id LIKE '%gemini%' LIMIT 1").fetchall()
secret_json = aesgcm.decrypt(rows[0][2], rows[0][1], None).decode()
cookie = json.loads(secret_json).get('cookie', '')

req = urllib.request.Request('https://gemini.google.com/app', headers={'Cookie': cookie, 'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36'})
with urllib.request.urlopen(req) as resp:
    html = resp.read().decode('utf-8', errors='ignore')

bl = re.search(r'"cfb2h":"([^"]+)"', html)
bl = bl.group(1) if bl else ''
at = re.search(r'"SNlM0e":"([^"]+)"', html)
at = at.group(1) if at else ''
fsid = re.search(r'"FdrFJe":"([^"]+)"', html)
fsid = fsid.group(1) if fsid else ''
print('Tokens:', {'bl': bl[:10], 'at': at[:10], 'fsid': fsid[:10]})

sapisid = ''
for part in cookie.split(';'):
    if 'SAPISID=' in part:
        sapisid = part.split('SAPISID=')[1].strip()
        break
t = int(time.time())
sha = hashlib.sha1(f'{t} {sapisid} https://gemini.google.com'.encode()).hexdigest()
auth = f'SAPISIDHASH {t}_{sha}'

prompt = 'Generate an image of a red apple on a table'
req_data = [None, json.dumps([[prompt, 0, None, [], None, None, 0], ['en'], ['', '', ''], None, None, None, [0], 1, None, None, None, None, None, []])]
body = urllib.parse.urlencode({'f.req': json.dumps(req_data), 'at': at})

url = f'https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl={bl}&hl=en&_reqid=12345&rt=c'
if fsid:
    url += f'&f.sid={fsid}'

headers = {
    'Content-Type': 'application/x-www-form-urlencoded;charset=utf-8',
    'Cookie': cookie,
    'Authorization': auth,
    'X-Same-Domain': '1',
    'Origin': 'https://gemini.google.com',
    'Referer': 'https://gemini.google.com/app',
    'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36',
    'x-goog-ext-525001261-jspb': '[1,null,null,null,"71c2d248d3b102ff"]'
}

req = urllib.request.Request(url, data=body.encode(), headers=headers)
with urllib.request.urlopen(req, timeout=30) as resp:
    raw = resp.read().decode('utf-8', errors='ignore')

print('Raw response length:', len(raw))
with open('/tmp/gemini_raw_img.txt', 'w', encoding='utf-8') as f:
    f.write(raw)

for m in re.finditer(r'https?://[^\s"\'\\]+', raw):
    u = m.group(0)
    if any(k in u for k in ['googleusercontent', 'image', 'photo', 'gg', 'content']):
        print('URL MATCH:', u)
