import json, os, sqlite3, sys, time, uuid
from pathlib import Path
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from playwright.sync_api import sync_playwright

ACCOUNTS = [
    ("copilot@maika2611.onmicrosoft.com", "H(495416680947uk"),
    ("copilot022@maika2611.onmicrosoft.com", "B(097700442140oq"),
    ("it1@8883t3.onmicrosoft.com", "N%728081950128aj"),
    ("tiktoktts1@maika2611.onmicrosoft.com", "W*185728548592ol"),
    ("tiktoktts2@maika2611.onmicrosoft.com", "R%619174403172up"),
    ("tiktoktts3@maika2611.onmicrosoft.com", "N$244697822285op"),
]

DB_PATH = "/media/reup/Data_sv4/Sever_provider/data/orchestrator.db"
MASTER_KEY = bytes.fromhex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
PROFILES_BASE = Path("/media/reup/Data_sv4/Sever_provider/data/profiles/copilot")
PROFILES_BASE.mkdir(parents=True, exist_ok=True)

def save_to_db(email: str, token_url: str):
    aesgcm = AESGCM(MASTER_KEY)
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute("SELECT id FROM identities WHERE lower(email) = ?", (email.lower(),))
    row = c.fetchone()
    if row:
        ident_id = row[0]
    else:
        ident_id = f"copilot-{uuid.uuid4().hex[:16]}"
        conn.execute("""
            INSERT INTO identities (id, provider_group, email, normalized_email, priority, global_status)
            VALUES (?, 'copilot', ?, ?, 1, 'active')
        """, (ident_id, email, email.lower()))
    
    cred_id = f"{ident_id}-copilot_ws"
    conn.execute("""
        INSERT INTO credentials (id, identity_id, domain, status, max_concurrency, pool_role, last_error_kind, consecutive_errors)
        VALUES (?, ?, 'copilot_ws', 'HEALTHY', 3, 'AUTO', '', 0)
        ON CONFLICT(id) DO UPDATE SET
            status = 'HEALTHY',
            last_error_kind = '',
            consecutive_errors = 0,
            updated_at = CURRENT_TIMESTAMP
    """, (cred_id, ident_id))
    
    secret_data = json.dumps({"token_url": token_url}).encode("utf-8")
    nonce = os.urandom(12)
    encrypted = aesgcm.encrypt(nonce, secret_data, None)
    conn.execute("""
        INSERT INTO credential_secrets (credential_id, encrypted_payload, nonce, key_version)
        VALUES (?, ?, ?, 1)
        ON CONFLICT(credential_id) DO UPDATE SET
            encrypted_payload = excluded.encrypted_payload,
            nonce = excluded.nonce,
            key_version = 1
    """, (cred_id, encrypted, nonce))
    conn.commit()
    conn.close()
    print(f"✅ Successfully saved and activated in DB: {email} -> {cred_id}")

def login_account(email: str, password: str) -> bool:
    safe_name = email.split("@")[0]
    profile_dir = PROFILES_BASE / safe_name
    print(f"\n==========================================")
    print(f"[*] Processing: {email}")
    print(f"[*] Profile Dir: {profile_dir}")
    
    captured_ws = []
    
    with sync_playwright() as p:
        browser = p.chromium.launch_persistent_context(
            str(profile_dir),
            executable_path="/usr/bin/google-chrome",
            headless=True,
            args=[
                "--no-sandbox",
                "--disable-dev-shm-usage",
                "--disable-gpu",
                "--disable-blink-features=AutomationControlled",
                "--window-size=1280,800"
            ]
        )
        page = browser.pages[0] if browser.pages else browser.new_page()
        
        def on_ws(ws):
            u = ws.url
            if "Chathub" in u and "access_token" in u:
                print(f"🎉 BẮT ĐƯỢC CHATHUB URL CHO {email}!")
                captured_ws.append(u)
        
        page.on("websocket", on_ws)
        
        try:
            print("[*] Navigating to https://m365.cloud.microsoft/chat...")
            page.goto("https://m365.cloud.microsoft/chat", wait_until="domcontentloaded", timeout=45000)
            time.sleep(3)
            
            # Step 1: Check Email input
            print(f"[*] Page URL: {page.url[:80]} | Title: {page.title()}")
            email_input = page.query_selector("input[type='email'], input[name='loginfmt']")
            if email_input:
                print("[*] Typing email...")
                email_input.fill(email)
                page.click("input[type='submit'], #idSIButton9")
                time.sleep(3)
            
            # Step 2: Check Password input
            pwd_input = page.query_selector("input[type='password'], input[name='passwd']")
            if pwd_input:
                print("[*] Typing password...")
                pwd_input.fill(password)
                page.click("input[type='submit'], #idSIButton9")
                time.sleep(3)
            
            # Step 3: Check 'Stay signed in?' / 'Next' buttons
            try:
                stay_btn = page.query_selector("#idSIButton9, input[value='Yes'], button:has-text('Yes'), input[value='Next']")
                if stay_btn:
                    print("[*] Confirming stay signed in...")
                    stay_btn.click()
                    time.sleep(3)
            except Exception as e:
                print("[*] No stay signed in dialog:", e)
                
            # Step 4: Wait for landing on chat and capture WS
            print("[*] Waiting for Copilot websocket connection...")
            start_wait = time.time()
            while time.time() - start_wait < 35:
                if captured_ws:
                    break
                # Try trigger editor if idle
                try:
                    ed = page.query_selector("#m365-chat-editor-target-element, [contenteditable='true'], textarea")
                    if ed:
                        ed.click()
                        ed.fill("hi")
                        btn = page.query_selector("button[aria-label*='Send' i], button[type='submit']")
                        if btn and not btn.is_disabled():
                            btn.click()
                            print("[*] Triggered send button")
                except Exception:
                    pass
                time.sleep(2)
                
        except Exception as exc:
            print(f"[-] Error during login: {exc}")
        finally:
            browser.close()
            
    if captured_ws:
        save_to_db(email, captured_ws[0])
        return True
    else:
        print(f"❌ Không bắt được token cho {email}")
        return False

def main():
    results = {}
    for email, pwd in ACCOUNTS:
        ok = login_account(email, pwd)
        results[email] = ok
        time.sleep(2)
        
    print("\n================ FINAL REPORT ================")
    for email, ok in results.items():
        print(f"{email:40} : {'SUCCESS ✅' if ok else 'FAILED ❌'}")

if __name__ == "__main__":
    main()
