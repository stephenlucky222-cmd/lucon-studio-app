# Checks the real offline captions: starts Lucon Link with Whisper and sends a spoken sentence.
# Results are written as GitHub notices so they can be read without the full log.
import sys, os, time, json, subprocess, urllib.request, wave, traceback
def note(kind, msg): print('::%s::%s' % (kind, str(msg).replace('%', '%25').replace('\r', '').replace('\n', '%0A')), flush=True)
link, wdir, wav = sys.argv[1], sys.argv[2], sys.argv[3]
ok = False; out = []; p = None
try:
    out.append('files: ' + ', '.join(sorted(os.listdir(wdir))) + ' | models: ' + ', '.join(sorted(os.listdir(os.path.join(wdir, 'models')))))
    w = wave.open(wav); out.append('wav: %d Hz, %d ch, %d bytes/sample, %.1f s' % (w.getframerate(), w.getnchannels(), w.getsampwidth(), w.getnframes() / w.getframerate()))
    pcm = w.readframes(w.getnframes()); open(os.path.join(os.path.dirname(wav), 'speech.pcm'), 'wb').write(pcm)
    logf = open(os.path.join(os.path.dirname(wav), 'link.log'), 'w')
    p = subprocess.Popen([link, '-no-browser', '-app', '-port', '9110', '-whisper', os.path.abspath(wdir), '-config', os.path.join(os.path.dirname(wav), 'cfg.json')], stdin=subprocess.PIPE, stdout=logf, stderr=subprocess.STDOUT)
    def get(path, data=None):
        r = urllib.request.Request('http://127.0.0.1:9110' + path, data=data, headers={'Origin': 'https://events.luconhouse.com', 'Content-Type': 'application/octet-stream'})
        try:
            with urllib.request.urlopen(r, timeout=240) as f: return json.loads(f.read())
        except urllib.error.HTTPError as e: return {'http': e.code, 'body': e.read().decode('utf8', 'replace')}
    st = None
    for i in range(40):
        try: st = get('/api/whisper'); break
        except Exception: time.sleep(0.5)
    out.append('status: %s' % st)
    t0 = time.time(); r1 = get('/api/whisper?lang=en', pcm); out.append('first (loads the model): %s in %.1f s' % (r1, time.time() - t0))
    t0 = time.time(); r2 = get('/api/whisper?lang=en&prompt=church', pcm); out.append('second: %s in %.1f s' % (r2, time.time() - t0))
    txt = (r2.get('text') or '').lower(); ok = 'faith' in txt and 'hoped' in txt
except Exception:
    out.append(traceback.format_exc())
finally:
    if p:
        try: p.stdin.close()
        except Exception: pass
        time.sleep(2); p.kill()
    try: out.append('link log: ' + open(os.path.join(os.path.dirname(wav), 'link.log')).read()[-1500:])
    except Exception: pass
note('notice' if ok else 'error', ('WHISPER TEST PASSED' if ok else 'WHISPER TEST FAILED') + '\n' + '\n'.join(out))
sys.exit(0 if ok else 1)
