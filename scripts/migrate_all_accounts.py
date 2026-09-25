#!/usr/bin/env python3
"""Migration script to import and deduplicate accounts into Sever_provider SQLite DB.

Imports:
  1. 9 Gemini Web accounts from /media/reup/Data_sv4/geminigo/data/cookies.json
  2. 11 Colab accounts from ~/.config/colab-cli/ (merging duplicates with Gemini under unified Google identities)
  3. 6 Copilot M365 accounts from /media/reup/Data_sv4/geminigo/data/copilot/copilot_accounts.json
  4. 9 TikTok accounts from /media/reup/Data_sv4/TTS_Omivoices/data/tts.db
  5. Default admin API key

All secrets (cookies, tokens, session IDs) are encrypted via AES-GCM-256 using MASTER_KEY.
"""
import base64
import glob
import hashlib
import json
import os
import re
import sqlite3
import sys
import time
from pathlib import Path
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

DEFAULT_MASTER_KEY = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
MASTER_KEY_HEX = os.environ.get("MASTER_KEY", DEFAULT_MASTER_KEY)
DB_PATH = os.environ.get("DB_PATH", "data/orchestrator.db")

aesgcm = AESGCM(bytes.fromhex(MASTER_KEY_HEX))


def encrypt_secret(secret_obj: dict) -> tuple[bytes, bytes]:
    data = json.dumps(secret_obj).encode("utf-8")
    nonce = os.urandom(12)
    payload = aesgcm.encrypt(nonce, data, None)
    return payload, nonce


def init_db(conn: sqlite3.Connection, schema_path: str):
    if os.path.exists(schema_path):
        with open(schema_path, "r", encoding="utf-8") as f:
            conn.executescript(f.read())
    conn.execute("PRAGMA journal_mode = WAL")
    conn.execute("PRAGMA foreign_keys = ON")


def migrate_gemini_and_colab(conn: sqlite3.Connection):
    print("\n--- 1. Migrating Gemini & Colab Accounts (Deduplicating Google Identities) ---")

    # Map Gemini profiles
    gemini_mapping = {
        "Account-1": "thanhchuapi1@gmail.com",
        "Account-2": "thanhchuapi2@gmail.com",
        "Account-3": "thanhchuapi3@gmail.com",
        "Account-4": "thanhchuapi5@gmail.com",
        "Account-5": "thanhchuapi0@gmail.com",
        "Account-6": "thanhchuapi6@gmail.com",
        "Account-7": "thanhchuapi7@gmail.com",
        "Account-8": "thanhchuapi8@gmail.com",
        "Account-9": "thanhchuapi9@gmail.com",
    }

    cookies_path = "/media/reup/Data_sv4/geminigo/data/cookies.json"
    gemini_items = []
    if os.path.exists(cookies_path):
        with open(cookies_path, "r", encoding="utf-8") as f:
            gemini_items = json.load(f)
    print(f"Loaded {len(gemini_items)} Gemini cookies from {cookies_path}")

    # Track created Google identities by email
    google_identities = {}

    for item in gemini_items:
        aid = item.get("id")
        email = gemini_mapping.get(aid, f"gemini-{aid.lower()}@gmail.com")
        cookie_str = item.get("cookie", "")
        if not cookie_str:
            continue

        ident_id = f"google-{hashlib.md5(email.encode()).hexdigest()[:16]}"
        google_identities[email.lower()] = ident_id

        # Insert or update identity
        conn.execute("""
            INSERT INTO identities (id, provider_group, email, normalized_email, priority, global_status)
            VALUES (?, 'google', ?, ?, 1, 'active')
            ON CONFLICT(id) DO UPDATE SET email=excluded.email, normalized_email=excluded.normalized_email, global_status='active'
        """, (ident_id, email, email.lower()))

        # Credential gemini_web
        cred_id = f"{ident_id}-gemini_web"
        conn.execute("""
            INSERT INTO credentials (id, identity_id, domain, status, max_concurrency, pool_role)
            VALUES (?, ?, 'gemini_web', 'HEALTHY', 4, 'AUTO')
            ON CONFLICT(id) DO UPDATE SET status='HEALTHY', max_concurrency=4
        """, (cred_id, ident_id))

        payload, nonce = encrypt_secret({"cookie": cookie_str})
        conn.execute("""
            INSERT INTO credential_secrets (credential_id, encrypted_payload, nonce, key_version)
            VALUES (?, ?, ?, 1)
            ON CONFLICT(credential_id) DO UPDATE SET encrypted_payload=excluded.encrypted_payload, nonce=excluded.nonce
        """, (cred_id, payload, nonce))

        print(f" -> [Gemini] {aid} -> Identity: {email} (ID: {ident_id})")

    # Colab Accounts
    colab_dir = os.path.expanduser("~/.config/colab-cli")
    colab_files = glob.glob(os.path.join(colab_dir, "token_*.json"))
    print(f"\nScanning {len(colab_files)} Colab token files from {colab_dir}...")

    for cpath in sorted(colab_files):
        fname = os.path.basename(cpath)
        name = fname.removeprefix("token_").removesuffix(".json")
        email = ""
        try:
            dec = base64.urlsafe_b64decode(name + "==").decode()
            if "@" in dec:
                email = dec
        except Exception:
            pass
        if not email and "_at_" in name:
            l, r = name.split("_at_", 1)
            email = l + "@" + r.replace("_", ".")
        if not email:
            email = name + "@gmail.com"

        email_norm = email.lower()
        with open(cpath, "r", encoding="utf-8") as f:
            token_json = json.load(f)

        if email_norm in google_identities:
            ident_id = google_identities[email_norm]
            print(f" -> [Colab MERGED with Gemini] {email} -> Existing Identity ID: {ident_id}")
        else:
            ident_id = f"google-{hashlib.md5(email_norm.encode()).hexdigest()[:16]}"
            google_identities[email_norm] = ident_id
            conn.execute("""
                INSERT INTO identities (id, provider_group, email, normalized_email, priority, global_status)
                VALUES (?, 'google', ?, ?, 1, 'active')
                ON CONFLICT(id) DO UPDATE SET email=excluded.email, normalized_email=excluded.normalized_email, global_status='active'
            """, (ident_id, email, email_norm))
            print(f" -> [Colab NEW Identity] {email} -> Created Identity ID: {ident_id}")

        cred_id = f"{ident_id}-google_colab"
        conn.execute("""
            INSERT INTO credentials (id, identity_id, domain, status, max_concurrency, pool_role)
            VALUES (?, ?, 'google_colab', 'HEALTHY', 4, 'AUTO')
            ON CONFLICT(id) DO UPDATE SET status='HEALTHY', max_concurrency=4
        """, (cred_id, ident_id))

        payload, nonce = encrypt_secret(token_json)
        conn.execute("""
            INSERT INTO credential_secrets (credential_id, encrypted_payload, nonce, key_version)
            VALUES (?, ?, ?, 1)
            ON CONFLICT(credential_id) DO UPDATE SET encrypted_payload=excluded.encrypted_payload, nonce=excluded.nonce
        """, (cred_id, payload, nonce))

        conn.execute("""
            INSERT INTO account_cooldowns (identity_id, state, cooldown_until, last_used_at)
            VALUES (?, 'READY', 0, 0)
            ON CONFLICT(identity_id) DO UPDATE SET state='READY'
        """, (ident_id,))


def migrate_copilot(conn: sqlite3.Connection):
    print("\n--- 2. Migrating Copilot M365 Accounts ---")
    cpath = "/media/reup/Data_sv4/geminigo/data/copilot/copilot_accounts.json"
    if not os.path.exists(cpath):
        print(f"Copilot accounts file not found at {cpath}")
        return

    with open(cpath, "r", encoding="utf-8") as f:
        data = json.load(f)
    print(f"Found {len(data)} Copilot accounts in {cpath}")

    for cid, acc in data.items():
        user_email = acc.get("user") or f"{cid.lower()}@copilot.local"
        token_url = acc.get("token_url", "")
        if not token_url:
            print(f" -> Skipping {cid}: No token_url")
            continue

        ident_id = f"copilot-{hashlib.md5(user_email.encode()).hexdigest()[:16]}"
        conn.execute("""
            INSERT INTO identities (id, provider_group, email, normalized_email, priority, global_status)
            VALUES (?, 'copilot', ?, ?, 1, 'active')
            ON CONFLICT(id) DO UPDATE SET email=excluded.email, normalized_email=excluded.normalized_email, global_status='active'
        """, (ident_id, user_email, user_email.lower()))

        cred_id = f"{ident_id}-copilot_ws"
        conn.execute("""
            INSERT INTO credentials (id, identity_id, domain, status, max_concurrency, pool_role)
            VALUES (?, ?, 'copilot_ws', 'HEALTHY', 3, 'SESSION')
            ON CONFLICT(id) DO UPDATE SET status='HEALTHY', max_concurrency=3
        """, (cred_id, ident_id))

        payload, nonce = encrypt_secret({"token_url": token_url})
        conn.execute("""
            INSERT INTO credential_secrets (credential_id, encrypted_payload, nonce, key_version)
            VALUES (?, ?, ?, 1)
            ON CONFLICT(credential_id) DO UPDATE SET encrypted_payload=excluded.encrypted_payload, nonce=excluded.nonce
        """, (cred_id, payload, nonce))

        print(f" -> [Copilot] {cid} -> User: {user_email} (ID: {ident_id})")


def migrate_tiktok(conn: sqlite3.Connection):
    print("\n--- 3. Migrating TikTok Accounts ---")
    tts_db_path = "/media/reup/Data_sv4/TTS_Omivoices/data/tts.db"
    if not os.path.exists(tts_db_path):
        print(f"TTS Omivoices db not found at {tts_db_path}")
        return

    tts_conn = sqlite3.connect(tts_db_path)
    cursor = tts_conn.execute("SELECT id, name, data, status FROM tiktok_accounts")
    rows = cursor.fetchall()
    print(f"Found {len(rows)} TikTok accounts in {tts_db_path}")

    for row in rows:
        tid, name, raw_data, status = row
        session_id = ""
        try:
            dj = json.loads(raw_data)
            session_id = dj.get("sessionid") or dj.get("session_id", "")
        except Exception:
            pass

        if not session_id:
            print(f" -> Skipping TikTok account {name}: No session_id found")
            continue

        email = f"{name}@tiktok.internal"
        ident_id = f"tiktok-{name}"

        conn.execute("""
            INSERT INTO identities (id, provider_group, email, normalized_email, priority, global_status)
            VALUES (?, 'tiktok', ?, ?, 1, 'active')
            ON CONFLICT(id) DO UPDATE SET email=excluded.email, normalized_email=excluded.normalized_email, global_status='active'
        """, (ident_id, email, email.lower()))

        cred_id = f"{ident_id}-tiktok_api"
        conn.execute("""
            INSERT INTO credentials (id, identity_id, domain, status, max_concurrency, pool_role)
            VALUES (?, ?, 'tiktok_api', 'HEALTHY', 4, 'AUTO')
            ON CONFLICT(id) DO UPDATE SET status='HEALTHY', max_concurrency=4
        """, (cred_id, ident_id))

        payload, nonce = encrypt_secret({"session_id": session_id})
        conn.execute("""
            INSERT INTO credential_secrets (credential_id, encrypted_payload, nonce, key_version)
            VALUES (?, ?, ?, 1)
            ON CONFLICT(credential_id) DO UPDATE SET encrypted_payload=excluded.encrypted_payload, nonce=excluded.nonce
        """, (cred_id, payload, nonce))

        print(f" -> [TikTok] {name} -> session_id: {session_id[:12]}... (Identity: {ident_id})")


def add_admin_api_key(conn: sqlite3.Connection):
    print("\n--- 4. Provisioning Admin API Key ---")
    admin_key = "sk-sp-admin-key-production"
    h = hashlib.sha256(admin_key.encode("utf-8")).digest()
    key_hash = base64.urlsafe_b64encode(h).decode().rstrip("=")

    conn.execute("""
        INSERT INTO api_keys (id, name, key_hash, active, scopes)
        VALUES ('key-admin-prod', 'Production Admin Key', ?, 1, 'all')
        ON CONFLICT(id) DO UPDATE SET key_hash=excluded.key_hash, active=1
    """, (key_hash,))
    print(f" -> Provisioned Admin API Key: {admin_key}")


def main():
    os.makedirs(os.path.dirname(os.path.abspath(DB_PATH)), exist_ok=True)
    print(f"Connecting to database: {DB_PATH} (WAL mode)")
    conn = sqlite3.connect(DB_PATH)

    schema_file = os.path.join(os.path.dirname(__file__), "..", "internal", "db", "schema.sql")
    init_db(conn, schema_file)

    migrate_gemini_and_colab(conn)
    migrate_copilot(conn)
    migrate_tiktok(conn)
    add_admin_api_key(conn)

    conn.commit()

    # Summary
    cur = conn.cursor()
    cur.execute("SELECT count(*) FROM identities")
    total_idents = cur.fetchone()[0]
    cur.execute("SELECT count(*) FROM credentials")
    total_creds = cur.fetchone()[0]
    cur.execute("SELECT domain, count(*) FROM credentials GROUP BY domain")
    domain_counts = cur.fetchall()

    print("\n" + "=" * 65)
    print("MIGRATION HOÀN TẤT THÀNH CÔNG!")
    print(f"Tổng số Identities (đã khử trùng): {total_idents}")
    print(f"Tổng số Credentials: {total_creds}")
    for d, c in domain_counts:
        print(f" - Domain {d:<15}: {c} credentials")
    print("=" * 65)


if __name__ == "__main__":
    main()
