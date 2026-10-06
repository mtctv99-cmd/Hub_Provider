import time, json
from pathlib import Path
from playwright.sync_api import sync_playwright

profile_dir = Path("/media/reup/Data_sv4/Sever_provider/data/profiles/copilot/copilot")

# Tạo 1 ảnh test nhỏ
test_img_path = Path("/tmp/test_image.png")
import base64
# 1x1 png
test_img_path.write_bytes(base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="))

with sync_playwright() as p:
    browser = p.chromium.launch_persistent_context(
        user_data_dir=str(profile_dir),
        headless=True,
        args=["--no-sandbox", "--disable-gpu"]
    )
    page = browser.pages[0] if browser.pages else browser.new_page()
    
    file_upload_requests = []
    ws_sent_frames = []
    
    def on_req(req):
        if any(k in req.url for k in ["copilotuploads", "drive", "upload", "files", "attachment"]):
            headers = dict(req.headers)
            # hide long bearer
            if "authorization" in headers:
                headers["authorization"] = headers["authorization"][:30] + "..."
            file_upload_requests.append({
                "method": req.method,
                "url": req.url,
                "headers": headers,
                "post_data": req.post_data[:200] if req.post_data else None
            })
            
    def on_ws(ws):
        def on_msg(payload):
            if isinstance(payload, str) and ("test_image" in payload or "adaptiveCards" in payload or "entityAnnotationTypes" in payload or "file" in payload.lower()):
                ws_sent_frames.append(payload[:500])
        ws.on("framesent", on_msg)

    page.on("request", on_req)
    page.on("websocket", on_ws)
    
    print("Opening chat...")
    page.goto("https://m365.cloud.microsoft/chat", timeout=60000)
    time.sleep(5)
    
    file_input = page.query_selector("input#upload-file-button")
    if file_input:
        print("Found file input, setting file...")
        file_input.set_input_files(str(test_img_path))
        time.sleep(5)
        print("File uploaded, checking requests...")
        
        # Trigger send
        ed = page.query_selector("#m365-chat-editor-target-element, [contenteditable='true'], textarea")
        if ed:
            ed.click()
            ed.fill("Anh nay la gi?")
            time.sleep(1)
            btn = page.query_selector("button[aria-label*='Send' i], button[type='submit']")
            if btn and not btn.is_disabled():
                btn.click()
                print("Clicked Send!")
                time.sleep(5)
                
    browser.close()
    print("\n--- UPLOAD REQUESTS ---")
    print(json.dumps(file_upload_requests, indent=2))
    print("\n--- WS FRAMES SENT ---")
    for f in ws_sent_frames:
        print(f)
