#!/usr/bin/env python3
"""Run full 23-slot benchmark and verify exact load distribution across GPU 0, GPU 1, and Colab T4."""
import json
import subprocess
import time
import requests

SERVER_URL = "http://192.168.1.117:18080/v1/audio/speech/batch"
AUTH_KEY = "sk-sp-unlimited-master-key"
SSH_HOST = "reup@192.168.1.117"

ITEMS = [
    # 8 Vietnamese items (piper:ngochuyennew)
    {"id": "vi-1", "model": "piper:ngochuyennew", "input": "Xin chào, chào mừng bạn đến với hệ thống chuyển đổi văn bản thành giọng nói."},
    {"id": "vi-2", "model": "piper:ngochuyennew", "input": "Hôm nay thời tiết rất đẹp và thích hợp cho các hoạt động ngoài trời."},
    {"id": "vi-3", "model": "piper:ngochuyennew", "input": "Hệ thống máy chủ đang xử lý hàng loạt yêu cầu tổng hợp âm thanh với tốc độ cao."},
    {"id": "vi-4", "model": "piper:ngochuyennew", "input": "Mô hình trí tuệ nhân tạo có khả năng phát âm chuẩn xác từng ngữ điệu tiếng Việt."},
    {"id": "vi-5", "model": "piper:ngochuyennew", "input": "Chúng tôi đang thực hiện kiểm thử tải song song hai mươi ba luồng đồng thời."},
    {"id": "vi-6", "model": "piper:ngochuyennew", "input": "Hiệu năng xử lý trên GPU giúp giảm đáng kể thời gian phản hồi của dịch vụ."},
    {"id": "vi-7", "model": "piper:ngochuyennew", "input": "Dữ liệu âm thanh xuất ra đạt chuẩn chất lượng phòng thu với định dạng sóng âm."},
    {"id": "vi-8", "model": "piper:ngochuyennew", "input": "Công nghệ học sâu mở ra nhiều triển vọng to lớn cho các ứng dụng tương lai."},

    # 8 Thai items (piper:th_TH-mms_female-medium)
    {"id": "th-1", "model": "piper:th_TH-mms_female-medium", "input": "สวัสดีครับ ยินดีต้อนรับเข้าสู่ระบบแปลงข้อความเป็นเสียงพูด"},
    {"id": "th-2", "model": "piper:th_TH-mms_female-medium", "input": "วันนี้อากาศสดใส เหมาะสำหรับการทำงานและการพักผ่อนอย่างยิ่ง"},
    {"id": "th-3", "model": "piper:th_TH-mms_female-medium", "input": "ระบบกำลังประมวลผลข้อความชุดใหญ่ด้วยความเร็วสูงมาก"},
    {"id": "th-4", "model": "piper:th_TH-mms_female-medium", "input": "เทคโนโลยีปัญญาประดิษฐ์ช่วยให้การสื่อสารเป็นไปอย่างมีประสิทธิภาพ"},
    {"id": "th-5", "model": "piper:th_TH-mms_female-medium", "input": "การทดสอบการทำงานพร้อมกันยี่สิบสามงานเสร็จสิ้นด้วยดี"},
    {"id": "th-6", "model": "piper:th_TH-mms_female-medium", "input": "โมเดลเสียงภาษาไทยทำงานได้อย่างราบรื่นและเป็นธรรมชาติ"},
    {"id": "th-7", "model": "piper:th_TH-mms_female-medium", "input": "คุณภาพเสียงที่ได้มีความคมชัดและตรงตามมาตรฐานสากล"},
    {"id": "th-8", "model": "piper:th_TH-mms_female-medium", "input": "ขอขอบคุณทุกท่านที่ให้ความร่วมมือในการพัฒนาโครงการนี้"},

    # 7 Khmer items (piper:khmer_tts_v3)
    {"id": "km-1", "model": "piper:khmer_tts_v3", "input": "សួស្តី សូមស្វាគមន៍មកកាន់ប្រព័ន្ធបម្លែងអត្ថបទទៅជាសំឡេង"},
    {"id": "km-2", "model": "piper:khmer_tts_v3", "input": "ថ្ងៃនេះអាកាសធាតុល្អណាស់ សម្រាប់ការបំពេញការងារនិងសម្រាកលំហែកាយ"},
    {"id": "km-3", "model": "piper:khmer_tts_v3", "input": "ប្រព័ន្ធកំពុងដំណើរការបម្លែងសំឡេងជាបាច់ដោយស្វ័យប្រវត្តិ"},
    {"id": "km-4", "model": "piper:khmer_tts_v3", "input": "បច្ចេកវិទ្យាបញ្ញាសិប្បនិម្មិតជួយសម្រួលដល់ការទំនាក់ទំនងកាន់តែប្រសើរ"},
    {"id": "km-5", "model": "piper:khmer_tts_v3", "input": "ការសាកល្បងដំណើរការដំណាលគ្នាចំនួនម្ភៃបីស្លុតទទួលបានជោគជ័យ"},
    {"id": "km-6", "model": "piper:khmer_tts_v3", "input": "សំឡេងភាសាខ្មែរត្រូវបានសំយោគយ៉ាងច្បាស់លាស់និងមានគុណភាពខ្ពស់"},
    {"id": "km-7", "model": "piper:khmer_tts_v3", "input": "សូមអរគុណយ៉ាងជ្រាលជ្រៅចំពោះកិច្ចសហការដ៏ល្អប្រសើរនេះ"},
]

def run_ssh(cmd):
    res = subprocess.run(["ssh", SSH_HOST, cmd], capture_output=True, text=True, check=True)
    return res.stdout.strip()

def main():
    print("=== STARTING 23-SLOT MULTILINGUAL BATCH BENCHMARK ===")
    
    # 1. Get baseline time from server
    server_time = run_ssh("date +'%Y-%m-%d %H:%M:%S'")
    print(f"Server baseline timestamp: {server_time}")
    time.sleep(1)

    headers = {
        "Authorization": f"Bearer {AUTH_KEY}",
        "Content-Type": "application/json",
    }
    payload = {"items": ITEMS}

    # 2. Send batch request
    t0 = time.perf_counter()
    resp = requests.post(SERVER_URL, headers=headers, json=payload, timeout=60)
    t1 = time.perf_counter()
    batch_latency_ms = (t1 - t0) * 1000.0

    assert resp.status_code == 200, f"Batch request failed: {resp.status_code} {resp.text}"
    resp_data = resp.json()
    results = resp_data.get("results", resp_data) if isinstance(resp_data, dict) else resp_data
    assert len(results) == 23, f"Expected 23 results, got {len(results)}"

    # 3. Analyze results
    success_count = sum(1 for r in results if r.get("status") in ("success", "ok"))
    assert success_count == 23, f"Not all items succeeded: {success_count}/23"

    latencies = [r.get("latency_ms", 0) for r in results]
    avg_item_latency_ms = sum(latencies) / len(latencies)
    items_per_second = len(results) / (batch_latency_ms / 1000.0)
    success_rate = f"{(success_count / len(results)) * 100:.1f}%"

    print("\n=== PER-ITEM LATENCY BREAKDOWN ===")
    for r in results:
        print(f"Item {r.get('id'):5} | index={r.get('index'):2} | latency={r.get('latency_ms'):3}ms | status={r.get('status')} | b64_bytes={len(r.get('audio_base64', ''))}")

    # 4. Query node completed counts
    time.sleep(1)
    gpu0_cmd = f"journalctl --user -u sever-tts-gpu0.service --since '{server_time}' | grep -c 'completed req_' || true"
    gpu1_cmd = f"journalctl --user -u sever-tts-gpu1.service --since '{server_time}' | grep -c 'completed req_' || true"
    colab_cmd = f"journalctl --user -u sever-colab-tts.service --since '{server_time}' | grep -c 'completed req_' || true"

    gpu0_count = int(run_ssh(gpu0_cmd))
    gpu1_count = int(run_ssh(gpu1_cmd))
    colab_count = int(run_ssh(colab_cmd))
    total_distributed = gpu0_count + gpu1_count + colab_count

    print("\n=== CLUSTER WORKER LOAD DISTRIBUTION ===")
    print(f"GPU 0 (RTX 3080, 10 slots):       {gpu0_count} items")
    print(f"GPU 1 (RTX 3060 Ti, 10 slots):    {gpu1_count} items")
    print(f"Colab T4 (3 slots):               {colab_count} items")
    print(f"Total processed across 3 nodes:   {total_distributed} items (expected 23)")

    worker_dist_str = f"GPU 0: {gpu0_count}/10 slots, GPU 1: {gpu1_count}/10 slots, Colab T4: {colab_count}/3 slots"

    summary = {
        "total_items": len(results),
        "batch_latency_ms": round(batch_latency_ms, 2),
        "avg_item_latency_ms": round(avg_item_latency_ms, 2),
        "items_per_second": round(items_per_second, 2),
        "worker_distribution": worker_dist_str,
        "success_rate": success_rate
    }

    print("\n=== FINAL STRUCTURED METRICS ===")
    print(json.dumps(summary, indent=2))

if __name__ == "__main__":
    main()
