"""Offline contract tests: python -m unittest discover -s colab -p 'test_*.py'."""
import base64
import io
import json
import threading
import time
import tempfile
from pathlib import Path
import unittest
import wave
from unittest.mock import patch

from combined_worker import (CombinedWorker, PiperVoice, registration, make_wav, KhmerVoice,
                             model_path, configured_models, resolve_model_id, sha256_path,
                             verify_manifest)


class FakeChat:
    def __init__(self):
        self.started = threading.Event()
        self.release = threading.Event()

    def stream(self, messages, max_tokens, temperature):
        self.started.set()
        self.release.wait(2)
        yield {'choices': [{'text': 'hello', 'finish_reason': None}], 'usage': {}}
        yield {'choices': [{'text': '', 'finish_reason': 'stop'}],
               'usage': {'prompt_tokens': 3, 'completion_tokens': 1, 'total_tokens': 4}}


class FakeVoice:
    def __init__(self):
        self.calls = 0
        self.started = threading.Event()
        self.release = threading.Event()

    def synthesize(self, text):
        self.calls += 1
        self.started.set()
        self.release.wait(2)
        return make_wav([0, 100, -100], 22050)


class WorkerTests(unittest.TestCase):
    def setUp(self):
        self.llm = FakeChat()
        self.voices = {name: FakeVoice() for name in ('ngochuyennew', 'th_TH-mms_female-medium', 'khmer_tts_v3')}
        self.worker = CombinedWorker('gemma-4-colab', self.llm, self.voices, queue_size=4)
        self.events = []
        self.lock = threading.Lock()

    def send(self, event):
        with self.lock:
            self.events.append(event)

    def wait_for(self, request_id, typ):
        until = time.monotonic() + 3
        while time.monotonic() < until:
            with self.lock:
                found = [e for e in self.events if e.get('request_id') == request_id and e['type'] == typ]
            if found:
                return found[-1]
            time.sleep(.01)
        self.fail(f'{request_id} did not produce {typ}: {self.events}')

    def tearDown(self):
        self.llm.release.set()
        for voice in self.voices.values():
            voice.release.set()
        self.worker.close()

    def test_registrations_partition_master_capacity(self):
        llm = registration('worker', 'configured-chat', self.voices, 'llm', 'not-logged')
        tts = registration('worker', 'configured-chat', self.voices, 'tts', 'not-logged',
                           configured_tts_models=['piper:khmer_tts_v3', 'piper:ngochuyennew'])
        self.assertEqual((llm['models'], llm['capabilities'], llm['max_concurrency']),
                         (['configured-chat'], ['chat', 'streaming'], 4))
        self.assertEqual(tts['models'], ['piper:khmer_tts_v3', 'piper:ngochuyennew'])
        self.assertEqual(tts['capabilities'], ['audio_speech', 'tts'])
        self.assertEqual(tts['max_concurrency'], 2)
        self.assertNotEqual(llm['worker_id'], tts['worker_id'])

        # Test explicit 20 TTS worker concurrency
        tts_20 = registration('worker', 'configured-chat', self.voices, 'tts', 'not-logged',
                              configured_tts_models=['piper:khmer_tts_v3', 'piper:ngochuyennew'],
                              tts_concurrency=20)
        self.assertEqual(tts_20['max_concurrency'], 20)

    def test_configured_models_and_model_id_selection(self):
        with tempfile.TemporaryDirectory() as td:
            config = Path(td) / 'models.yaml'
            config.write_text("""models:
  custom-chat:
    provider: google_colab
    capabilities: [chat, streaming]
  piper:ngochuyennew:
    provider: piper_local
    capabilities: [audio_speech]
  piper:khmer_tts_v3:
    provider: piper_local
    capabilities: [audio_speech]
  piper:not-enabled:
    provider: piper_local
    capabilities: [audio_speech]
""", encoding='utf-8')
            llm_models, tts_models = configured_models(config)
            self.assertEqual(llm_models, ['custom-chat'])
            self.assertEqual(tts_models, ['piper:khmer_tts_v3', 'piper:ngochuyennew'])
            self.assertEqual(resolve_model_id(config, None), 'custom-chat')
            self.assertEqual(resolve_model_id(config, 'custom-chat'), 'custom-chat')
            with self.assertRaisesRegex(ValueError, 'not configured'):
                resolve_model_id(config, 'gemma-4-colab')

    def test_config_ambiguity_requires_explicit_model(self):
        with tempfile.TemporaryDirectory() as td:
            config = Path(td) / 'models.yaml'
            config.write_text("""models:
  model-a:
    provider: google_colab
    capabilities: [chat]
  model-b:
    provider: google_colab
    capabilities: [chat]
""", encoding='utf-8')
            with self.assertRaisesRegex(ValueError, 'multiple google_colab chat models'):
                resolve_model_id(config, None)

    def test_concurrent_chat_and_tts_no_shared_queue(self):
        self.worker.dispatch({'type': 'chat_request', 'request_id': 'ai', 'model': 'gemma-4-colab',
                              'messages': [{'role': 'user', 'content': 'hi'}], 'stream': True}, self.send, 'llm')
        self.assertTrue(self.llm.started.wait(1))
        self.worker.dispatch({'type': 'chat_request', 'request_id': 'tts', 'model': 'piper:ngochuyennew',
                              'voice_id': 'ngochuyennew', 'text': 'Xin chào'}, self.send, 'tts')
        self.assertTrue(self.voices['ngochuyennew'].started.wait(1))
        self.voices['ngochuyennew'].release.set()
        result = self.wait_for('tts', 'audio_result')
        self.assertEqual(result['mime'], 'audio/wav')
        with wave.open(io.BytesIO(base64.b64decode(result['audio_base64']))) as wav:
            self.assertEqual(wav.getframerate(), 22050)
        self.llm.release.set()
        self.assertEqual(self.wait_for('ai', 'token_end')['usage']['total_tokens'], 4)

    def test_voice_serializes_and_llm_stays_independent(self):
        for rid in ('a', 'b'):
            self.worker.dispatch({'type': 'chat_request', 'request_id': rid, 'model': 'piper:khmer_tts_v3', 'text': 'ខ្មែរ'}, self.send, 'tts')
        self.assertTrue(self.voices['khmer_tts_v3'].started.wait(1))
        self.assertEqual(self.voices['khmer_tts_v3'].calls, 1)
        self.voices['khmer_tts_v3'].release.set()
        self.wait_for('b', 'audio_result')
        self.assertEqual(self.voices['khmer_tts_v3'].calls, 2)

    def test_bad_routes_and_empty_inputs_fail_closed(self):
        self.worker.dispatch({'type': 'chat_request', 'request_id': 'x', 'model': 'piper:ngochuyennew', 'text': 'hi'}, self.send, 'llm')
        self.worker.dispatch({'type': 'chat_request', 'request_id': 'y', 'model': 'piper:unknown', 'text': 'hi'}, self.send, 'tts')
        self.worker.dispatch({'type': 'chat_request', 'request_id': 'z', 'model': 'gemma-4-colab', 'messages': []}, self.send, 'llm')
        for rid in ('x', 'y', 'z'):
            self.assertIn('error', self.wait_for(rid, 'error'))

    def test_chat_nonstream_yields_terminal_text_not_duplicate_tokens(self):
        self.llm.release.set()
        self.worker.dispatch({'type': 'chat_request', 'request_id': 'r', 'model': 'gemma-4-colab',
                              'messages': [{'role': 'user', 'content': 'hi'}], 'stream': False}, self.send, 'llm')
        self.assertEqual(self.wait_for('r', 'job_result')['text'], 'hello')
        self.assertFalse(any(e['type'] == 'token' for e in self.events))

    def test_queue_full_error(self):
        self.worker = CombinedWorker('gemma-4-colab', self.llm, self.voices, queue_size=1)
        for rid in ('a', 'b', 'c'):
            self.worker.dispatch({'type': 'chat_request', 'request_id': rid, 'model': 'piper:ngochuyennew', 'text': 'hi'}, self.send, 'tts')
            if rid == 'a':
                self.assertTrue(self.voices['ngochuyennew'].started.wait(1))
        self.assertIn('queue', self.wait_for('c', 'error')['error'])

    def test_backend_error_is_terminal(self):
        self.voices['ngochuyennew'].synthesize = lambda text: (_ for _ in ()).throw(RuntimeError('failure'))
        self.worker.dispatch({'type': 'chat_request', 'request_id': 'err', 'model': 'piper:ngochuyennew', 'text': 'hi'}, self.send, 'tts')
        self.assertIn('failure', self.wait_for('err', 'error')['error'])

    def test_khmer_onnx_vocabulary_pipeline(self):
        import numpy as np
        class Input:
            def __init__(self, name): self.name = name
        class Sess:
            def get_inputs(self): return [Input(x) for x in ('x', 'x_lengths')]
            def run(self, outputs, feeds):
                self.feeds = feeds
                return [np.array([[[0., .5, -.5]]], dtype=np.float32)]
        config = {'vocab': ['@', 'ខ', '្', 'ម', 'រ'], 'blank': '@',
                  'add_blank': True, 'sample_rate': 22050}
        session = Sess()
        voice = KhmerVoice(session, config, sample_rate=24000)
        with wave.open(io.BytesIO(voice.synthesize('ខ្មែរ'))) as wav:
            self.assertEqual((wav.getframerate(), wav.getnframes()), (24000, 3))
        self.assertEqual(session.feeds['x'].tolist(), [[0, 1, 0, 2, 0, 3, 0, 4, 0]])
        self.assertEqual(session.feeds['x_lengths'].tolist(), [9])

    def test_piper_voice_synthesize_wav_or_synthesize(self):
        class MockVoiceWithWav:
            def synthesize_wav(self, text, wav):
                wav.setnchannels(1)
                wav.setsampwidth(2)
                wav.setframerate(22050)
                wav.writeframes(b'\x00\x00')
        class MockVoiceLegacy:
            def synthesize(self, text, wav):
                wav.setnchannels(1)
                wav.setsampwidth(2)
                wav.setframerate(16000)
                wav.writeframes(b'\x01\x00')

        pv1 = PiperVoice.__new__(PiperVoice)
        pv1.voice = MockVoiceWithWav()
        wav_bytes1 = pv1.synthesize('hello')
        with wave.open(io.BytesIO(wav_bytes1)) as w:
            self.assertEqual(w.getframerate(), 22050)

        pv2 = PiperVoice.__new__(PiperVoice)
        pv2.voice = MockVoiceLegacy()
        wav_bytes2 = pv2.synthesize('hello')
        with wave.open(io.BytesIO(wav_bytes2)) as w:
            self.assertEqual(w.getframerate(), 16000)

    def test_model_path_resolution(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            nested_dir = root / 'ngoc_huyen'
            nested_dir.mkdir()
            nested = nested_dir / 'ngochuyennew.onnx'
            nested.touch()
            self.assertEqual(model_path(root, 'ngochuyennew'), nested)

            flat = root / 'th_TH-mms_female-medium.onnx'
            flat.touch()
            self.assertEqual(model_path(root, 'th_TH-mms_female-medium'), flat)

            with self.assertRaises(FileNotFoundError):
                model_path(root, 'non_existent')

    def test_sha256_file_directory_and_manifest(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            f = root / 'asset.gguf'
            f.write_bytes(b'GGUF123')
            import hashlib
            self.assertEqual(sha256_path(f), hashlib.sha256(b'GGUF123').hexdigest())
            cfg = root / 'khmer_tts_v3.json'
            cfg.write_text('{"vocab":["@"]}')
            cfg_hash = sha256_path(cfg)
            manifest = root / 'manifest.json'
            manifest.write_text(json.dumps({'gguf': {'path': str(f), 'sha256': sha256_path(f)},
                                            'khmer_tts_v3_config': {'path': str(cfg), 'sha256': cfg_hash}}))
            result = verify_manifest(manifest, {'gguf': f, 'khmer_tts_v3_config': cfg})
            self.assertEqual(result['khmer_tts_v3_config']['sha256'], cfg_hash)
            manifest.write_text(json.dumps({'gguf': {'sha256': '0' * 64}}))
            with self.assertRaisesRegex(ValueError, 'asset hash mismatch'):
                verify_manifest(manifest, {'gguf': f})

    def test_khmer_config_missing_fails_without_model_download(self):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            onnx = root / 'khmer.onnx'
            onnx.touch()
            config = root / 'khmer.json'
            with self.assertRaisesRegex(FileNotFoundError, 'character config missing'):
                KhmerVoice.load(onnx, config, 22050, ['CPUExecutionProvider'])


if __name__ == '__main__':
    unittest.main()
