import io
import json
import threading
import time
import unittest
import wave
from unittest.mock import patch

from verify_combined import benchmark, gpu_memory_mib, summarize


class FakeLLM:
    def stream(self, messages, max_tokens, temperature):
        time.sleep(.02)
        yield {'choices': [{'delta': {'content': 'ok'}}]}
        yield {'choices': [{'delta': {}, 'finish_reason': 'stop'}], 'usage': {'total_tokens': 2}}


class FakeVoice:
    def synthesize(self, text):
        time.sleep(.01)
        out = io.BytesIO()
        with wave.open(out, 'wb') as wav:
            wav.setnchannels(1)
            wav.setsampwidth(2)
            wav.setframerate(22050)
            wav.writeframes(b'\x00\x00' * 220)
        return out.getvalue()


class VerificationTests(unittest.TestCase):
    def test_benchmark_concurrent_and_reports_metrics(self):
        from combined_worker import CombinedWorker, VOICE_IDS
        w = CombinedWorker('gemma-4-colab', FakeLLM(), {v: FakeVoice() for v in VOICE_IDS})
        try:
            result = benchmark(w, rounds=2, sample_interval=.01, gpu_sampler=lambda: 100)
            self.assertEqual(result['chat']['count'], 2)
            self.assertEqual(result['tts']['count'], 6)
            self.assertEqual(result['errors'], [])
            self.assertEqual(result['gpu_peak_mib'], 100)
            self.assertGreater(result['chat']['p50_ms'], 0)
        finally:
            w.close()

    def test_summary_empty_and_percentile(self):
        self.assertEqual(summarize([])['count'], 0)
        self.assertEqual(summarize([1., 3., 5.])['p50_ms'], 3.)

    def test_cpu_without_nvidia_smi(self):
        with patch('verify_combined.subprocess.run', side_effect=FileNotFoundError):
            self.assertIsNone(gpu_memory_mib())


if __name__ == '__main__':
    unittest.main()
