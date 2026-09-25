import os
import json
import glob
import re

def extract_gemini_accounts(geminigo_dir):
    cookies_path = os.path.join(geminigo_dir, "data", "cookies.json")
    if not os.path.exists(cookies_path):
        return {}
    with open(cookies_path, "r", encoding="utf-8") as f:
        cookies_data = json.load(f)

    # Map profile_dir to email
    acc_map = {}
    for acc in cookies_data:
        aid = acc.get("id")
        profile_path = os.path.join(geminigo_dir, "data", "profiles", aid, "Default", "Preferences")
        email = None
        if os.path.exists(profile_path):
            try:
                with open(profile_path, "r", encoding="utf-8", errors="ignore") as pf:
                    content = pf.read()
                    emails = re.findall(r'[a-zA-Z0-9._%+-]+@gmail\.com', content)
                    if emails:
                        email = emails[0]
            except:
                pass
        
        if not email:
            email = f"unknown_{aid.lower()}@gmail.com"

        acc_map[email] = {
            "gemini_cookie": acc.get("cookie"),
            "sapisid": acc.get("sapisid"),
            "google_api_key": acc.get("google_api_key"),
            "profile_dir": acc.get("profile_dir")
        }
    return acc_map

def extract_colab_accounts(colab_dir):
    token_files = glob.glob(os.path.join(colab_dir, "token_*.json"))
    colab_map = {}
    for tf in token_files:
        basename = os.path.basename(tf)
        # e.g. token_thanhchuapi01_at_gmail_com.json
        match = re.match(r'token_(.+)_at_gmail_com\.json', basename)
        if match:
            email = match.group(1) + "@gmail.com"
        else:
            email = basename.replace("token_", "").replace(".json", "")
        
        try:
            with open(tf, "r", encoding="utf-8") as f:
                token_data = json.load(f)
            colab_map[email] = {
                "token_file": tf,
                "token_data": token_data
            }
        except:
            pass
    return colab_map

def build_unified_registry(geminigo_dir="/media/reup/Data_sv4/geminigo", colab_dir="/home/reup/.config/colab-cli"):
    gemini = extract_gemini_accounts(geminigo_dir)
    colab = extract_colab_accounts(colab_dir)
    
    all_emails = sorted(list(set(list(gemini.keys()) + list(colab.keys()))))
    unified = []
    
    for idx, email in enumerate(all_emails, 1):
        ident_id = f"acc_g_{idx:02d}"
        item = {
            "id": ident_id,
            "provider_group": "google",
            "email": email,
            "priority": 1,
            "credentials": []
        }
        
        if email in gemini:
            item["credentials"].append({
                "domain": "gemini_web",
                "max_concurrency": 4,
                "secret": gemini[email]
            })
            
        if email in colab:
            item["credentials"].append({
                "domain": "google_colab",
                "max_concurrency": 8,
                "secret": colab[email]
            })
            
        unified.append(item)
    return unified

if __name__ == "__main__":
    import sys
    g_dir = sys.argv[1] if len(sys.argv) > 1 else "/media/reup/Data_sv4/geminigo"
    c_dir = sys.argv[2] if len(sys.argv) > 2 else "/home/reup/.config/colab-cli"
    data = build_unified_registry(g_dir, c_dir)
    print(json.dumps(data, indent=2))
