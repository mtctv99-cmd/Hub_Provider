"""Tests for LocalTTSWorker."""
import base64
import io
import sys
import threading
import time
import unittest
import wave
from pathlib import Path

# Add scripts directory to path
SCRIPTS_DIR = Path(__file__).resolve().parent
REPO_ROOT = SCRIPTS_DIR.parent
sys.path.insert(0, str(SCRIPTS_DIR))
sys.path.insert(0, str(REPO_ROOT))

from local_tts_worker import LocalTTSWorker
from colab.combined_worker import make_wav


class FakeVoice:
    def __init__(self, sample_rate=22050):
        self.sample_rate = sample_rate
        self.calls = 0

    def synthesize(self, text):
        self.calls += 1
        return make_wav([0, 100, -100], self.sample_rate)


class LocalTTSWorkerTests(unittest.TestCase):
    def setUp(self):
        self.voices = {
            'ngochuyennew': FakeVoice(),
            'th_TH-mms_female-medium': FakeVoice(),
            'khmer_tts_v3': FakeVoice(),
        }
        self.worker = LocalTTSWorker(self.voices, queue_size=32)
        self.events = []
        self.lock = threading.Lock()

    def tearDown(self):
        self.worker.close()

    def send(self, evt):
        with self.lock:
            self.events.append(evt)

    def wait_for(self, request_id, typ, timeout=3.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            with self.lock:
                for e in self.events:
                    if e.get('request_id') == request_id and e.get('type') == typ:
                        return e
            time.sleep(0.01)
        self.fail(f'Timed out waiting for {typ} on {request_id}')

    def test_dispatch_all_three_voices_success(self):
        for vid in ('ngochuyennew', 'th_TH-mms_female-medium', 'khmer_tts_v3'):
            rid = f'req-{vid}'
            self.worker.dispatch({
                'type': 'tts_request',
                'request_id': rid,
                'model': f'piper:{vid}',
                'voice_id': vid,
                'text': 'Test utterance',
            }, self.send)
            res = self.wait_for(rid, 'audio_result')
            self.assertEqual(res['mime'], 'audio/wav')
            raw_audio = base64.b64decode(res['audio_base64'])
            with wave.open(io.BytesIO(raw_audio)) as w:
                self.assertEqual(w.getframerate(), 22050)
                self.assertEqual(w.getnchannels(), 1)

    def test_unsupported_voice_fails_closed(self):
        self.worker.dispatch({
            'type': 'tts_request',
            'request_id': 'bad-1',
            'model': 'piper:unknown',
            'voice_id': 'unknown',
            'text': 'hello',
        }, self.send)
        err = self.wait_for('bad-1', 'error')
        self.assertIn('unsupported voice', err['error'])

    def test_empty_text_fails_closed(self):
        self.worker.dispatch({
            'type': 'tts_request',
            'request_id': 'bad-2',
            'model': 'piper:ngochuyennew',
            'voice_id': 'ngochuyennew',
            'text': '   ',
        }, self.send)
        err = self.wait_for('bad-2', 'error')
        self.assertIn('empty text', err['error'])

    def test_ping_pong(self):
        pong = []
        self.worker.dispatch({'type': 'ping'}, lambda m: pong.append(m))
        self.assertEqual(pong, [{'type': 'pong'}])

    def test_concurrent_20_tts_jobs(self):
        # Dispatch 20 parallel TTS jobs across the 3 voices
        voices = ['ngochuyennew', 'th_TH-mms_female-medium', 'khmer_tts_v3']
        rids = [f'bench-20-{i}' for i in range(20)]
        for i, rid in enumerate(rids):
            vid = voices[i % len(voices)]
            self.worker.dispatch({
                'type': 'tts_request',
                'request_id': rid,
                'model': f'piper:{vid}',
                'voice_id': vid,
                'text': f'Utterance #{i}',
            }, self.send)
        for rid in rids:
            res = self.wait_for(rid, 'audio_result', timeout=5.0)
            self.assertEqual(res['mime'], 'audio/wav')
            self.assertTrue(res.get('done'))


if __name__ == '__main__':
    unittest.main()
