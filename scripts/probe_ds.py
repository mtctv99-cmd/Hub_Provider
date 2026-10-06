import subprocess, time, json, urllib.request, shutil

profile = '/tmp/ds_probe_profile2'
shutil.rmtree(profile, ignore_errors=True)

cmd = [
    '/usr/bin/google-chrome',
    '--headless=new',
    '--remote-debugging-port=9241',
    '--remote-allow-origins=*',
    '--user-agent=Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36',
    '--disable-blink-features=AutomationControlled',
    '--no-sandbox',
    '--user-data-dir=' + profile,
    '--no-first-run',
    '--no-default-browser-check',
    'https://chat.deepseek.com/sign_in'
]
p = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(8)

try:
    with urllib.request.urlopen('http://127.0.0.1:9241/json/list', timeout=5) as resp:
        tabs = json.loads(resp.read().decode())
        print('TABS:', [(t.get('type'), t.get('url')) for t in tabs])
        ws_url = None
        for t in tabs:
            if t.get('type') == 'page' and 'deepseek.com' in t.get('url', ''):
                ws_url = t['webSocketDebuggerUrl']
                break
        print('PICKED:', ws_url)
        import websocket
        ws = websocket.create_connection(ws_url, timeout=10)
        exprs = [
            'document.documentElement.outerHTML.slice(0, 4000)',
        ]
        for i, e in enumerate(exprs):
            ws.send(json.dumps({'id': 10 + i, 'method': 'Runtime.evaluate', 'params': {'expression': e, 'returnByValue': True}}))
            res = json.loads(ws.recv())
            print(i, ':', res)
        ws.close()
finally:
    p.kill()
    p.wait()
