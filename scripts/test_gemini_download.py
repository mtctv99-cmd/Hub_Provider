import sqlite3, json, urllib.request, re
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

MASTER_KEY = bytes.fromhex('0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef')
aesgcm = AESGCM(MASTER_KEY)

conn = sqlite3.connect('/media/reup/Data_sv4/Sever_provider/data/orchestrator.db')
rows = conn.execute("SELECT credential_id, encrypted_payload, nonce FROM credential_secrets WHERE credential_id LIKE '%gemini%' LIMIT 1").fetchall()
cookie = json.loads(aesgcm.decrypt(rows[0][2], rows[0][1], None).decode()).get('cookie', '')

url = 'https://lh3.googleusercontent.com/gg-dl/AHiVA1mXzWvOf8X8b6RQGaX1BAnZ1cq2HKKgXGDlOS9GUhNfFuDQN5JjkNqjCcpMug3xVBq5qYhKPWXuYQlSwwT6331BHi2rOZKLRDQvsMi0BxK5QWAlUyviDdC_2Rl2wCoqD3TZ4oRzlGDjDDr1BikIMP41dpkhaYr-jsa5U2jKC4mvVgfI8g'
req = urllib.request.Request(url, headers={'Cookie': cookie, 'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36'})
try:
    with urllib.request.urlopen(req, timeout=10) as resp:
        data = resp.read()
        print('DOWNLOAD WITH COOKIE SUCCESS: size=', len(data), 'content-type=', resp.headers.get('content-type'))
except Exception as e:
    print('DOWNLOAD WITH COOKIE FAILED:', e)

with open('/tmp/gemini_raw_img.txt', 'r', encoding='utf-8') as f:
    raw = f.read()

c_urls = re.findall(r'https://contribution\.usercontent\.google\.com/download[^\s"\'\\]+', raw)
for cu in c_urls[:2]:
    print('TRY CONTRIBUTION URL:', cu[:60])
    req2 = urllib.request.Request(cu, headers={'Cookie': cookie, 'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36'})
    try:
        with urllib.request.urlopen(req2, timeout=10) as resp:
            data = resp.read()
            print('  CONTRIBUTION SUCCESS: size=', len(data), 'content-type=', resp.headers.get('content-type'))
    except Exception as e:
        print('  CONTRIBUTION FAILED:', e)
