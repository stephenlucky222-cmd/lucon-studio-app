# Checks the real offline captions: starts Lucon Link with Whisper and sends a spoken sentence.
import sys, os, time, json, subprocess, urllib.request, wave
link, wdir, wav = sys.argv[1], sys.argv[2], sys.argv[3]
w = wave.open(wav); assert w.getframerate() == 16000 and w.getnchannels() == 1 and w.getsampwidth() == 2, (w.getframerate(), w.getnchannels(), w.getsampwidth())
pcm = w.readframes(w.getnframes()); open(os.path.join(os.path.dirname(wav), 'speech.pcm'), 'wb').write(pcm)
p = subprocess.Popen([link, '-no-browser', '-app', '-port', '9110', '-whisper', wdir, '-config', os.path.join(os.path.dirname(wav), 'cfg.json')], stdin=subprocess.PIPE)
def get(path, data=None):
    r = urllib.request.Request('http://127.0.0.1:9110' + path, data=data, headers={'Origin': 'https://events.luconhouse.com', 'Content-Type': 'application/octet-stream'})
    with urllib.request.urlopen(r, timeout=180) as f: return json.loads(f.read())
ok = False
try:
    for i in range(40):
        try: st = get('/api/whisper'); break
        except Exception: time.sleep(0.5)
    print('status', st)
    t0 = time.time(); r1 = get('/api/whisper?lang=en', pcm); print('first (loads the model):', r1, round(time.time() - t0, 1), 's')
    t0 = time.time(); r2 = get('/api/whisper?lang=en&prompt=church', pcm); print('second:', r2, round(time.time() - t0, 1), 's')
    ok = 'faith' in r2.get('text', '').lower() and 'hoped' in r2.get('text', '').lower()
    print('WHISPER TEST', 'PASSED' if ok else 'FAILED')
finally:
    p.stdin.close(); time.sleep(2); p.kill()
sys.exit(0 if ok else 1)
