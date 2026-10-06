import time
from pathlib import Path
from playwright.sync_api import sync_playwright

profile_dir = Path("/media/reup/Data_sv4/Sever_provider/data/profiles/copilot/copilot")

with sync_playwright() as p:
    browser = p.chromium.launch_persistent_context(
        user_data_dir=str(profile_dir),
        headless=True,
        args=["--no-sandbox", "--disable-gpu"]
    )
    page = browser.pages[0] if browser.pages else browser.new_page()
    
    network_logs = []
    ws_frames = []
    
    def on_req(req):
        if "upload" in req.url or "file" in req.url or "blob" in req.url or "attachment" in req.url:
            network_logs.append(f"REQ: {req.method} {req.url}")
            
    def on_ws(ws):
        def on_msg_sent(payload):
            if "image" in str(payload).lower() or "file" in str(payload).lower():
                ws_frames.append(f"SENT: {payload[:300]}")
        ws.on("framesent", on_msg_sent)

    page.on("request", on_req)
    page.on("websocket", on_ws)
    
    print("Navigating to m365 chat...")
    page.goto("https://m365.cloud.microsoft/chat", timeout=60000)
    time.sleep(5)
    
    # Inspect inputs and buttons
    inputs = page.query_selector_all("input")
    for inp in inputs:
        print("INPUT:", inp.get_attribute("type"), inp.get_attribute("name"), inp.get_attribute("id"), inp.get_attribute("aria-label"))
        
    buttons = page.query_selector_all("button")
    for btn in buttons:
        aria = btn.get_attribute("aria-label") or btn.inner_text()
        if any(w in aria.lower() for w in ["file", "upload", "attach", "image", "add"]):
            print("BTN:", aria, btn.get_attribute("id"), btn.get_attribute("class")[:40])
            
    browser.close()
    print("DONE. Network logs:", network_logs[:10])
