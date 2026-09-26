#!/usr/bin/env python3
"""Load test POST /v1/audio/speech/batch with 23 concurrent items across 3 languages."""
import json
import time
import requests

SERVER_URL = "http://192.168.1.117:18080/v1/audio/speech/batch"
AUTH_KEY = "sk-sp-unlimited-master-key"

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

def main():
    print(f"Total items in batch: {len(ITEMS)}")
    headers = {
        "Authorization": f"Bearer {AUTH_KEY}",
        "Content-Type": "application/json",
    }
    payload = {"items": ITEMS}

    t0 = time.perf_counter()
    resp = requests.post(SERVER_URL, headers=headers, json=payload, timeout=60)
    t1 = time.perf_counter()

    batch_latency_ms = (t1 - t0) * 1000.0
    print(f"HTTP Status: {resp.status_code}")
    if resp.status_code != 200:
        print("Error response:", resp.text)
        return

    results = resp.json()
    print(f"Received results count: {len(results)}")

    success_count = sum(1 for r in results if r.get("status") == "success")
    latencies = [r.get("latency_ms", 0) for r in results if "latency_ms" in r]
    avg_item_latency_ms = (sum(latencies) / len(latencies)) if latencies else 0.0
    items_per_second = len(results) / (batch_latency_ms / 1000.0)

    print("\n--- PERFORMANCE SUMMARY ---")
    print(f"Total Items: {len(results)}")
    print(f"Success Count: {success_count}/{len(results)} ({success_count/len(results)*100:.1f}%)")
    print(f"Batch Latency: {batch_latency_ms:.2f} ms")
    print(f"Avg Item Latency: {avg_item_latency_ms:.2f} ms")
    print(f"Throughput: {items_per_second:.2f} items/sec")

    for r in results:
        audio_len = len(r.get("audio_base64", ""))
        print(f"[{r.get('id')}] index={r.get('index')} status={r.get('status')} latency={r.get('latency_ms')}ms mime={r.get('mime_type')} audio_b64_len={audio_len}")

if __name__ == "__main__":
    main()
