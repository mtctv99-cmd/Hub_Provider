import json, os, sqlite3, sys, time, uuid
from pathlib import Path
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from playwright.sync_api import sync_playwright

# Tài khoản lấy từ biến môi trường, không hardcode mật khẩu trong file.
# Cách dùng:
#   DEEPSEEK_ACCOUNTS_JSON='[["thanhchuapi1@gmail.com","pass1"],["thanhchuapi2@gmail.com","pass2"]]' python3 auto_deepseek_login.py
# Hoặc chạy dry-run kiểm tra cấu trúc trang đăng nhập (không cần tài khoản):
#   python3 auto_deepseek_login.py --dry-run
raw = os.environ.get("DEEPSEEK_ACCOUNTS_JSON", "[]")
try:
    ACCOUNTS = [(a[0], a[1]) for a in json.loads(raw)]
except Exception:
    print("DEEPSEEK_ACCOUNTS_JSON không hợp lệ, kỳ vọng mảng [[email, password], ...].")
    sys.exit(1)

DB_PATH = "/media/reup/Data_sv4/Sever_provider/data/orchestrator.db"
MASTER_KEY = bytes.fromhex("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
PROFILES_BASE = Path("/media/reup/Data_sv4/Sever_provider/data/profiles/deepseek")
PROFILES_BASE.mkdir(parents=True, exist_ok=True)

# Profile cũ đã lưu trong DB (source_ref) để tái dùng session còn sống.
PROFILE_BY_EMAIL = {
    "thanhchuapi1@gmail.com": "deepseek-c572d58d73f621a5",
    "thanhchuapi2@gmail.com": "deepseek-0e4c1ac441f7798c",
}

USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

def profile_for(email: str) -> Path:
    name = PROFILE_BY_EMAIL.get(email.lower())
    if name:
        return PROFILES_BASE / name
    safe = "".join(c if (c.isalnum() or c in "-_") else "_" for c in email)[:64]
    return PROFILES_BASE / safe

def save_to_db(email: str, token: str):
    aesgcm = AESGCM(MASTER_KEY)
    conn = sqlite3.connect(DB_PATH)
    c = conn.cursor()
    c.execute("SELECT id FROM identities WHERE lower(email) = ?", (email.lower(),))
    row = c.fetchone()
    if row:
        ident_id = row[0]
    else:
        ident_id = "deepseek-" + email.lower()
        conn.execute("""
            INSERT INTO identities (id, provider_group, email, normalized_email, priority, global_status)
            VALUES (?, 'deepseek', ?, ?, 1, 'active')
        """, (ident_id, email, email.lower()))

    cred_id = f"{ident_id}-deepseek_web"
    profile_dir = profile_for(email)
    conn.execute("""
        INSERT INTO credentials (id, identity_id, domain, status, max_concurrency, pool_role, source_ref, last_error_kind, consecutive_errors)
        VALUES (?, ?, 'deepseek_web', 'HEALTHY', 1, 'AUTO', ?, '', 0)
        ON CONFLICT(id) DO UPDATE SET
            status = 'HEALTHY',
            source_ref = excluded.source_ref,
            last_error_kind = '',
            consecutive_errors = 0,
            updated_at = CURRENT_TIMESTAMP
    """, (cred_id, ident_id, f"browser_profile:{profile_dir.name}"))

    secret_data = json.dumps({"token": token}).encode("utf-8")
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
    print(f"SUCCESS saved: {email} -> {cred_id} (profile {profile_dir.name})")

def dump_page_state(page, label: str):
    print(f"--- {label} ---")
    print("URL:", page.url[:100])
    print("Title:", page.title())
    try:
        body = (page.inner_text("body") or "")[:400]
        print("Body:", body.replace("\n", " | ")[:400])
    except Exception as e:
        print("Body read fail:", e)
    try:
        els = page.evaluate("""() => Array.from(document.querySelectorAll('button, input, a')).map(e => ({
            tag: e.tagName, type: e.type || '', text: (e.innerText || e.value || '').slice(0, 50)
        }))""")
        for e in els[:30]:
            print("  el:", e)
    except Exception as e:
        print("DOM dump fail:", e)

def try_capture_token(context) -> str:
    """Đọc userToken từ localStorage của tab chat.deepseek.com nếu đã đăng nhập."""
    for page in context.pages:
        try:
            if "deepseek.com" not in (page.url or ""):
                continue
            val = page.evaluate("() => localStorage.getItem('userToken') || ''")
            if not val:
                continue
            try:
                obj = json.loads(val)
                tok = obj.get("value") if isinstance(obj, dict) else val.strip('"')
            except Exception:
                tok = val.strip('"')
            if tok and tok != "null" and len(tok) > 20:
                return tok
        except Exception:
            continue
    return ""

def login_account(email: str, password: str, dry_run: bool = False) -> bool:
    profile_dir = profile_for(email)
    print(f"\n========== {email} (profile {profile_dir.name}) ==========")
    token = ""

    with sync_playwright() as p:
        context = p.chromium.launch_persistent_context(
            str(profile_dir),
            executable_path="/usr/bin/google-chrome",
            headless=True,
            user_agent=USER_AGENT,
            args=[
                "--no-sandbox",
                "--disable-dev-shm-usage",
                "--disable-gpu",
                "--disable-blink-features=AutomationControlled",
                "--window-size=1280,800",
            ],
        )
        try:
            page = context.pages[0] if context.pages else context.new_page()
            page.goto("https://chat.deepseek.com/sign_in", wait_until="domcontentloaded", timeout=45000)
            page.wait_for_timeout(5000)
            dump_page_state(page, "sign_in loaded")

            # Session cũ còn sống thì token đã có, không cần nhập pass.
            token = try_capture_token(context)
            if token:
                print("[*] Session cũ còn sống, bắt được token mà không cần login.")
                context.close()
                if not dry_run:
                    save_to_db(email, token)
                return True

            if dry_run:
                return False

            # Luồng login bằng email + password của DeepSeek (cả 2 ô nằm sẵn trên sign_in).
            print("[*] Nhập email...")
            email_input = page.query_selector("input[type='text']")
            if not email_input:
                print("FAIL: không tìm thấy ô email, xem dump ở trên.")
                return False
            email_input.fill(email)
            page.wait_for_timeout(1500)

            dump_page_state(page, "after email fill")

            # Ô password nằm sẵn ngay trên sign_in cùng ô email.
            pwd_input = page.query_selector("input[type='password']")
            if pwd_input:
                print("[*] Nhập password...")
                pwd_input.fill(password)
                page.wait_for_timeout(1000)
                try:
                    page.keyboard.press("Enter")
                    print("[*] Pressed Enter to submit login form")
                except Exception as e:
                    print("Enter press fail:", e)
                page.wait_for_timeout(5000)
                # Nếu Enter không submit, thử click nút submit có trong form.
                if not try_capture_token(context):
                    try:
                        page.click("button[type='submit'], div[role='button']:has-text('Log in')")
                        print("[*] Clicked submit button as fallback")
                    except Exception:
                        pass
                page.wait_for_timeout(5000)
            else:
                print("WARN: không thấy ô password (có thể DeepSeek gửi mã OTP về email thay vì password).")

            dump_page_state(page, "after password submit")

            # Chờ tối đa 60s cho token xuất hiện (người dùng giải captcha tay nếu có).
            start = time.time()
            while time.time() - start < 60:
                dump_page_state(page, f"waiting token ({int(time.time()-start)}s)")
                token = try_capture_token(context)
                if token:
                    break
                # Phát hiện captcha Cloudflare/CloudFront để báo rõ.
                try:
                    body = (page.inner_text("body") or "").lower()
                    if "captcha" in body or "verify you are human" in body or "choose all" in body:
                        print("CAPTCHA DETECTED: cần giải tay trên VNC, script đang chờ...")
                except Exception:
                    pass
                time.sleep(5)

        finally:
            context.close()

    if token:
        save_to_db(email, token)
        return True
    print(f"FAILED: không bắt được token cho {email}")
    return False

def main():
    dry_run = "--dry-run" in sys.argv
    if dry_run:
        print("Dry-run: chỉ kiểm tra cấu trúc trang sign_in, không cần tài khoản.")
        ok = login_account("dry-run@example.com", "", dry_run=True)
        print("Dry-run done, token captured:", ok)
        return
    if not ACCOUNTS:
        print("Chưa có tài khoản. Export DEEPSEEK_ACCOUNTS_JSON trước khi chạy.")
        sys.exit(1)
    results = {}
    for email, pwd in ACCOUNTS:
        try:
            results[email] = login_account(email, pwd)
        except Exception as e:
            print(f"ERROR {email}: {e}")
            results[email] = False
        time.sleep(2)
    print("\n================ FINAL REPORT ================")
    for email, ok in results.items():
        print(f"{email:40} : {'SUCCESS' if ok else 'FAILED'}")

if __name__ == "__main__":
    main()
