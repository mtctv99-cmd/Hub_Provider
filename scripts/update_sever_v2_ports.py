import sqlite3
import shutil
import os
import json
import hashlib

print("--- 1. Updating sever.db ---")
db_path = "/media/reup/Data_sv4/SVRV/sever_v2/sever/data/sever.db"
if os.path.exists(db_path):
    shutil.copyfile(db_path, db_path + ".bak-port18080")
    con = sqlite3.connect(db_path)
    cur = con.cursor()

    cur.execute("SELECT id, name, base_url, api_key FROM providers WHERE base_url LIKE '%8090%' OR base_url LIKE '%8081%';")
    for row in cur.fetchall():
        pid, name, url, key = row
        new_url = url.replace(":8090", ":18080").replace(":8081", ":18080")
        new_key = "sk-sp-unlimited-master-key" if not key or key == "omni-secret-key-123" else key
        cur.execute("UPDATE providers SET base_url=?, api_key=? WHERE id=?", (new_url, new_key, pid))
        print(f" -> Provider '{name}': {url} => {new_url} (Key: {new_key[:12]}...)")

    cur.execute("SELECT key, value FROM settings WHERE key='shared_tier_configs';")
    row = cur.fetchone()
    if row:
        val = row[1].replace(":8081", ":18080").replace(":8090", ":18080")
        cur.execute("UPDATE settings SET value=? WHERE key='shared_tier_configs'", (val,))
        print(" -> Setting 'shared_tier_configs' updated to port 18080")

    con.commit()
    con.close()

print("\n--- 2. Updating static.py ---")
static_py = "/media/reup/Data_sv4/SVRV/sever_v2/sever/config/static.py"
if os.path.exists(static_py):
    shutil.copyfile(static_py, static_py + ".bak-port18080")
    with open(static_py, "r") as f:
        code = f.read()
    code = code.replace('_OMNI_TTS_DEFAULT_BASE_URL = "http://localhost:8090"', '_OMNI_TTS_DEFAULT_BASE_URL = "http://localhost:18080"')
    code = code.replace('_OMNI_TTS_DEFAULT_API_KEY = "omni-secret-key-123"', '_OMNI_TTS_DEFAULT_API_KEY = "sk-sp-unlimited-master-key"')
    with open(static_py, "w") as f:
        f.write(code)
    print(" -> static.py updated (_OMNI_TTS_DEFAULT_BASE_URL = 'http://localhost:18080')")

print("\n--- 3. Updating ai_sentinel.py ---")
sentinel_py = "/media/reup/Data_sv4/SVRV/sever_v2/sever/upload_queue/ai_sentinel.py"
if os.path.exists(sentinel_py):
    shutil.copyfile(sentinel_py, sentinel_py + ".bak-port18080")
    with open(sentinel_py, "r") as f:
        scode = f.read()
    scode = scode.replace('"base_url": "http://localhost:8081/v1"', '"base_url": "http://localhost:18080/v1"')
    with open(sentinel_py, "w") as f:
        f.write(scode)
    print(" -> ai_sentinel.py updated (base_url = 'http://localhost:18080/v1')")

print("\n--- 4. Updating config_history.json & config_active.json ---")
history_json = "/media/reup/Data_sv4/SVRV/sever_v2/data/config/config_history.json"
active_json = "/media/reup/Data_sv4/SVRV/sever_v2/data/config/config_active.json"
if os.path.exists(history_json) and os.path.exists(active_json):
    shutil.copyfile(history_json, history_json + ".bak-port18080")
    shutil.copyfile(active_json, active_json + ".bak-port18080")

    with open(history_json) as f:
        hdata = json.load(f)

    for entry in hdata.get("entries", []):
        s = json.dumps(entry)
        s = s.replace(":8081", ":18080").replace(":8090", ":18080")
        new_entry = json.loads(s)
        entry.clear()
        entry.update(new_entry)

    # Recompute checksum for active entry 0
    active_entry = hdata["entries"][0]
    payload = json.dumps(active_entry["snapshot"], sort_keys=True, separators=(",", ":"))
    new_checksum = f"sha256:{hashlib.sha256(payload.encode()).hexdigest()}"
    active_entry["checksum"] = new_checksum

    with open(history_json, "w") as f:
        json.dump(hdata, f, indent=2)

    with open(active_json) as f:
        adata = json.load(f)

    adata["business_profile"]["checksum"] = new_checksum
    with open(active_json, "w") as f:
        json.dump(adata, f, indent=2)

    print(f" -> config_history.json & config_active.json updated (Checksum: {new_checksum})")

print("\nAll sever-v2 configurations successfully migrated to port 18080!")
