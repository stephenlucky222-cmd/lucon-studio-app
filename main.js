// Lucon Events and Lucon Studio for Mac and Windows (one code base, two apps; flavour.json says which one this is).
// Lucon Events: registration, check-in, badges, reports and the Studio. Lucon Studio: opens straight to the Studio.
// The website (events.luconhouse.com) in its own window with its own Chromium engine, Lucon Link built in
// (streaming, professional recording, PTZ cameras and offline captions with Whisper), a menu-bar / tray status,
// "start when the computer starts", and updates.
'use strict';
const { app, BrowserWindow, Menu, Tray, shell, session, dialog, ipcMain, desktopCapturer, systemPreferences, nativeImage, net } = require('electron');
const path = require('path');
const fs = require('fs');
const { spawn } = require('child_process');

const SITE = 'https://events.luconhouse.com';
const FLAVOUR = (() => { try { return JSON.parse(fs.readFileSync(path.join(__dirname, 'flavour.json'), 'utf8')).id === 'events' ? 'events' : 'studio'; } catch { return 'studio'; } })();
const EVENTS = FLAVOUR === 'events';
const NAME = EVENTS ? 'Lucon Events' : 'Lucon Studio';
const ICON = path.join(__dirname, 'assets', EVENTS ? 'icon-events-256.png' : 'icon-256.png');
// Lucon Events opens My Events; Lucon Studio opens My Events too, but each event opens straight in the Studio
const HOME = SITE + '/admin/index.html' + (EVENTS ? '' : '?app=studio');
const LINK = 'http://127.0.0.1:9110';
const REPO = 'stephenlucky222-cmd/lucon-studio-app';
const IS_MAC = process.platform === 'darwin', IS_WIN = process.platform === 'win32';
const SMOKE = process.env.LUCON_SMOKE || ''; // set by the automatic tests: a file to write results to

app.setName(NAME);
if (!SMOKE && !app.requestSingleInstanceLock()) { app.quit(); }
// the Studio draws the programme many times a second: never slow it down in the background
app.commandLine.appendSwitch('disable-background-timer-throttling');
app.commandLine.appendSwitch('disable-renderer-backgrounding');
app.commandLine.appendSwitch('disable-backgrounding-occluded-windows');
app.commandLine.appendSwitch('autoplay-policy', 'no-user-gesture-required');

/* ---------- small settings file ---------- */
const SETF = () => path.join(app.getPath('userData'), 'app-settings.json');
let SET = { last: '', bounds: null, maxed: true };
function loadSet() { try { SET = Object.assign(SET, JSON.parse(fs.readFileSync(SETF(), 'utf8'))); } catch {} }
function saveSet() { try { fs.writeFileSync(SETF(), JSON.stringify(SET, null, 2)); } catch {} }
function log(...a) { const line = new Date().toISOString() + ' ' + a.join(' ') + '\n'; try { fs.appendFileSync(path.join(app.getPath('userData'), 'app.log'), line); } catch {} if (!app.isPackaged || SMOKE) process.stdout.write(line); }

/* ---------- Lucon Link, built in ---------- */
const res = (...p) => app.isPackaged ? path.join(process.resourcesPath, ...p) : path.join(__dirname, 'res', ...p);
let link = null, linkState = null, linkOther = '';
function startLink() {
  const exe = res('link', IS_WIN ? 'lucon-link.exe' : 'lucon-link');
  if (!fs.existsSync(exe)) { log('Lucon Link not found at', exe); return; }
  try { fs.chmodSync(exe, 0o755); const ws = res('whisper', IS_WIN ? 'whisper-server.exe' : 'whisper-server'); if (fs.existsSync(ws)) fs.chmodSync(ws, 0o755); } catch {}
  link = spawn(exe, ['-no-browser', '-app', '-whisper', res('whisper')], { stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true });
  link.stdout.on('data', d => log('[link]', String(d).trim()));
  link.stderr.on('data', d => log('[link]', String(d).trim()));
  link.on('exit', (code) => { log('Lucon Link stopped', code); link = null; });
  link.on('error', e => log('Lucon Link error', e.message));
}
function stopLink() { if (!link) return; try { link.stdin.end(); } catch {} const l = link; setTimeout(() => { try { l.kill(); } catch {} }, 1500); }
async function linkGet(p) {
  return new Promise((resolve) => {
    const req = net.request({ url: LINK + p, method: 'GET' });
    let body = ''; const t = setTimeout(() => { try { req.abort(); } catch {} resolve(null); }, 2500);
    req.on('response', r => { r.on('data', c => body += c); r.on('end', () => { clearTimeout(t); try { resolve(JSON.parse(body)); } catch { resolve(null); } }); });
    req.on('error', () => { clearTimeout(t); resolve(null); });
    req.end();
  });
}
async function pollLink() {
  linkState = await linkGet('/api/state');
  linkOther = linkState && linkState.version && !link ? linkState.version : '';
  buildTray();
}

/* ---------- windows ---------- */
let win = null, tray = null, quitting = false;
const trusted = u => { try { const x = new URL(u); return x.origin === SITE || (x.protocol === 'file:' ) || x.origin === LINK; } catch { return false; } };
function webPrefs() {
  return { preload: path.join(__dirname, 'preload.js'), contextIsolation: true, sandbox: true, backgroundThrottling: false, spellcheck: false, autoplayPolicy: 'no-user-gesture-required' };
}
function createWindow(url) {
  const b = SET.bounds || { width: 1440, height: 900 };
  win = new BrowserWindow({ ...b, minWidth: 1024, minHeight: 680, backgroundColor: '#06080d', title: NAME, show: false, icon: ICON, webPreferences: webPrefs() });
  if (SET.maxed && !SMOKE) win.maximize();
  win.once('ready-to-show', () => win.show());
  win.on('page-title-updated', (e, t) => { e.preventDefault(); const n = String(t || '').replace(/\s*[·|–-]\s*Lucon.*$/i, '').trim(); win.setTitle(n && !/^lucon/i.test(n) ? NAME + ' — ' + n : NAME); });
  win.on('close', () => { try { SET.maxed = win.isMaximized(); if (!SET.maxed) SET.bounds = win.getBounds(); saveSet(); } catch {} });
  win.on('closed', () => { win = null; });
  wireContents(win.webContents);
  win.loadURL(url || startUrl()).catch(() => {});
  return win;
}
function startUrl() { return SET.last && SET.last.startsWith(SITE + '/') ? SET.last : HOME; }
function wireContents(wc) {
  // stay on our site; everything else opens in the normal browser
  wc.on('will-navigate', (e, u) => { if (!trusted(u)) { e.preventDefault(); shell.openExternal(u); } });
  wc.setWindowOpenHandler(({ url }) => {
    if (url === 'about:blank' || trusted(url)) return { action: 'allow', overrideBrowserWindowOptions: { backgroundColor: '#000000', autoHideMenuBar: true, icon: ICON, webPreferences: webPrefs() } };
    shell.openExternal(url); return { action: 'deny' };
  });
  wc.on('did-create-window', w => wireContents(w.webContents));
  wc.on('did-navigate', (e, u) => { if ((EVENTS ? /\/(workspace|station)\/[^?]*\?/ : /\/workspace\/studio\/index\.html\?/).test(u)) { SET.last = u; saveSet(); } });
  wc.on('did-fail-load', (e, code, desc, u, main) => {
    if (!main || code === -3) return; // -3 = aborted (normal when a page changes)
    log('Load failed', code, desc, u);
    wc.loadFile(path.join(__dirname, 'offline.html'), { query: { u, d: desc, n: NAME } }).catch(() => {});
  });
  wc.on('render-process-gone', (e, d) => { log('Page stopped', d.reason); if (d.reason !== 'clean-exit' && !quitting) setTimeout(() => { try { wc.reload(); } catch {} }, 800); });
}

/* ---------- cameras, microphones, screens, MIDI and Stream Deck ---------- */
const ALLOW = new Set(['media', 'display-capture', 'notifications', 'clipboard-read', 'clipboard-sanitized-write', 'fullscreen', 'midi', 'midiSysex', 'window-management', 'speaker-selection', 'pointerLock', 'keyboardLock', 'hid', 'serial', 'usb', 'storage-access', 'top-level-storage-access']);
function wirePermissions(ses) {
  ses.setPermissionRequestHandler((wc, perm, cb, d) => cb(ALLOW.has(perm) && trusted(d.requestingUrl || wc.getURL())));
  ses.setPermissionCheckHandler((wc, perm, origin) => ALLOW.has(perm) && trusted(origin || (wc && wc.getURL()) || ''));
  ses.setDevicePermissionHandler(d => trusted(d.origin));
  ses.on('select-hid-device', (e, d, cb) => { e.preventDefault(); cb(d.deviceList && d.deviceList[0] ? d.deviceList[0].deviceId : ''); });
  ses.on('select-usb-device', (e, d, cb) => { e.preventDefault(); cb(d.deviceList && d.deviceList[0] ? d.deviceList[0].deviceId : ''); });
  ses.on('select-serial-port', (e, list, wc, cb) => { e.preventDefault(); cb(list[0] ? list[0].portId : ''); });
  ses.setDisplayMediaRequestHandler((req, cb) => { pickScreen(req).then(cb).catch(() => cb({})); }, { useSystemPicker: true });
}
async function pickScreen(req) {
  const sources = await desktopCapturer.getSources({ types: ['screen', 'window'], thumbnailSize: { width: 320, height: 180 }, fetchWindowIcons: false });
  const list = sources.filter(s => !/^Lucon (Studio|Events)/.test(s.name) || s.id.startsWith('screen')).map(s => ({ id: s.id, name: s.name, screen: s.id.startsWith('screen'), img: s.thumbnail.toDataURL() }));
  return new Promise(resolve => {
    const p = new BrowserWindow({ width: 820, height: 600, parent: win || undefined, modal: !!win, resizable: true, minimizable: false, title: 'Share a screen or window', backgroundColor: '#0b0f17', webPreferences: { preload: path.join(__dirname, 'picker-preload.js'), contextIsolation: true, sandbox: true } });
    let done = false;
    const finish = (id, audio) => { if (done) return; done = true; const s = sources.find(x => x.id === id); try { p.close(); } catch {} resolve(s ? { video: s, ...(audio && IS_WIN ? { audio: 'loopback' } : {}) } : {}); };
    ipcMain.once('picker-done', (e, id, audio) => finish(id, audio));
    p.on('closed', () => finish(null));
    p.setMenuBarVisibility(false);
    p.loadFile(path.join(__dirname, 'picker.html')).then(() => p.webContents.send('picker-list', list, IS_WIN));
  });
}

/* ---------- menus ---------- */
function goHome() { if (!win) return createWindow(HOME); win.show(); win.loadURL(HOME); }
function openLinkSettings() { const w = new BrowserWindow({ width: 980, height: 760, title: 'Lucon Link settings', backgroundColor: '#0b0f17', webPreferences: { contextIsolation: true, sandbox: true } }); w.setMenuBarVisibility(false); w.loadURL(LINK + '/'); }
function loginOn() { try { return app.getLoginItemSettings().openAtLogin; } catch { return false; } }
function setLogin(on) { try { app.setLoginItemSettings({ openAtLogin: on, openAsHidden: false }); } catch {} buildTray(); buildMenu(); }
function buildMenu() {
  const t = [
    ...(IS_MAC ? [{ label: NAME, submenu: [{ role: 'about' }, { label: 'Check for updates…', click: () => checkUpdates(true) }, { type: 'separator' }, { label: 'Start when the computer starts', type: 'checkbox', checked: loginOn(), click: m => setLogin(m.checked) }, { type: 'separator' }, { role: 'hide' }, { role: 'hideOthers' }, { type: 'separator' }, { role: 'quit', label: 'Quit ' + NAME }] }] : []),
    { label: 'File', submenu: [{ label: 'Open ' + NAME, accelerator: 'CmdOrCtrl+O', click: () => { if (win) win.show(); else createWindow(); } }, { label: 'My events', accelerator: 'CmdOrCtrl+Shift+H', click: goHome }, { label: 'Lucon Link settings', click: openLinkSettings }, { type: 'separator' }, IS_MAC ? { role: 'close' } : { role: 'quit', label: 'Quit' }] },
    { label: 'Edit', submenu: [{ role: 'undo' }, { role: 'redo' }, { type: 'separator' }, { role: 'cut' }, { role: 'copy' }, { role: 'paste' }, { role: 'selectAll' }] },
    { label: 'View', submenu: [{ label: 'Back', accelerator: IS_MAC ? 'Cmd+[' : 'Alt+Left', click: () => { const wc = BrowserWindow.getFocusedWindow()?.webContents; if (wc && wc.navigationHistory.canGoBack()) wc.navigationHistory.goBack(); } }, { label: 'Reload', accelerator: 'CmdOrCtrl+R', click: () => BrowserWindow.getFocusedWindow()?.webContents.reload() }, { label: 'Reload (fresh copy)', accelerator: 'CmdOrCtrl+Shift+R', click: () => BrowserWindow.getFocusedWindow()?.webContents.reloadIgnoringCache() }, { type: 'separator' }, { role: 'resetZoom' }, { role: 'zoomIn' }, { role: 'zoomOut' }, { type: 'separator' }, { role: 'togglefullscreen' }, { label: 'Developer tools (for support)', accelerator: IS_MAC ? 'Alt+Cmd+I' : 'Ctrl+Shift+I', click: () => BrowserWindow.getFocusedWindow()?.webContents.toggleDevTools() }] },
    { label: 'Window', submenu: [{ role: 'minimize' }, ...(IS_MAC ? [{ role: 'zoom' }, { type: 'separator' }, { role: 'front' }] : [])] },
    { label: 'Help', submenu: [{ label: NAME + ' help', click: () => shell.openExternal(SITE + '/help/index.html') }, { label: 'Show the app log', click: () => shell.showItemInFolder(path.join(app.getPath('userData'), 'app.log')) }, ...(IS_MAC ? [] : [{ label: 'Check for updates…', click: () => checkUpdates(true) }, { label: 'Start when the computer starts', type: 'checkbox', checked: loginOn(), click: m => setLogin(m.checked) }])] }
  ];
  Menu.setApplicationMenu(Menu.buildFromTemplate(t));
}
function buildTray() {
  if (SMOKE) return;
  if (!tray) {
    const img = IS_MAC ? nativeImage.createFromPath(path.join(__dirname, 'assets', 'trayTemplate.png')) : nativeImage.createFromPath(path.join(__dirname, 'assets', 'tray-win.png'));
    if (IS_MAC) img.setTemplateImage(true);
    tray = new Tray(img); tray.setToolTip('Lucon Link');
    tray.on('click', () => { if (!IS_MAC) { if (win) win.show(); else createWindow(); } });
  }
  const s = linkState, ok = !!s, ff = s && s.ffmpeg, wh = s && s.whisper;
  if (IS_MAC) tray.setTitle(' Lucon Link');
  const whTxt = !wh ? '—' : ({ ready: 'ready', idle: 'ready', starting: 'starting…', missing: 'not installed', failed: 'problem' }[wh.state] || wh.state);
  const items = [
    { label: ok ? (linkOther ? '● Lucon Link ' + linkOther + ' (separate program) is running' : '● Lucon Link is running') : '○ Lucon Link is not running', enabled: false },
    { label: 'Streaming engine: ' + (!ok ? '—' : ff && ff.ok ? 'ready' : ff && ff.installing ? 'installing…' : 'needs FFmpeg'), enabled: false },
    { label: 'PTZ cameras: ' + (ok ? (s.ptz || []).length + ' saved' : '—'), enabled: false },
    { label: 'Offline captions: ' + whTxt, enabled: false },
    { type: 'separator' },
    { label: 'Open ' + NAME, accelerator: 'CmdOrCtrl+O', click: () => { if (win) { win.show(); win.focus(); } else createWindow(); } },
    { label: 'Lucon Link settings', click: openLinkSettings },
    { label: 'Start when the computer starts', type: 'checkbox', checked: loginOn(), click: m => setLogin(m.checked) },
    { label: 'Check for updates (v' + app.getVersion() + ')', click: () => checkUpdates(true) },
    { type: 'separator' },
    { label: 'Quit', accelerator: 'CmdOrCtrl+Q', click: () => app.quit() }
  ];
  tray.setContextMenu(Menu.buildFromTemplate(items));
}

/* ---------- updates ---------- */
const newer = (a, b) => { const x = String(a).replace(/^v/, '').split('.').map(Number), y = String(b).replace(/^v/, '').split('.').map(Number); for (let i = 0; i < 3; i++) { if ((x[i] || 0) > (y[i] || 0)) return true; if ((x[i] || 0) < (y[i] || 0)) return false; } return false; };
let upd = { busy: false, ready: false, told: '' };
async function latestRelease() {
  return new Promise(resolve => {
    const req = net.request({ url: `https://api.github.com/repos/${REPO}/releases/latest`, method: 'GET' });
    req.setHeader('User-Agent', NAME.replace(' ', '-')); req.setHeader('Accept', 'application/vnd.github+json');
    let body = ''; req.on('response', r => { r.on('data', c => body += c); r.on('end', () => { try { resolve(r.statusCode === 200 ? JSON.parse(body) : null); } catch { resolve(null); } }); });
    req.on('error', () => resolve(null)); req.end();
  });
}
async function checkUpdates(byHand) {
  if (upd.busy || SMOKE) return; upd.busy = true;
  try {
    const rel = await latestRelease();
    if (!rel || !rel.tag_name) { if (byHand) dialog.showMessageBox({ type: 'info', message: 'Could not check for updates', detail: 'Check the internet connection and try again.' }); return; }
    const v = rel.tag_name.replace(/^v/, '');
    if (!newer(v, app.getVersion())) { if (byHand) dialog.showMessageBox({ type: 'info', message: NAME + ' is up to date', detail: 'You have version ' + app.getVersion() + '.' }); return; }
    if (IS_WIN && app.isPackaged) {
      // Windows: download in the background, then offer a restart
      const { autoUpdater } = require('electron-updater');
      autoUpdater.autoDownload = true; autoUpdater.autoInstallOnAppQuit = true; autoUpdater.logger = { info: m => log('[update]', m), warn: m => log('[update]', m), error: m => log('[update]', m) };
      autoUpdater.once('update-downloaded', async () => {
        const r = await dialog.showMessageBox({ type: 'info', buttons: ['Restart now', 'Later'], defaultId: 0, cancelId: 1, message: 'Update ready — restart', detail: NAME + ' ' + v + ' is ready. Restart now, or it will be installed when you quit. Do not restart during a live show.' });
        if (r.response === 0) { quitting = true; autoUpdater.quitAndInstall(); }
      });
      autoUpdater.once('error', e => { log('[update] failed', e && e.message); if (byHand) shell.openExternal(rel.html_url); });
      await autoUpdater.checkForUpdates();
      if (byHand) dialog.showMessageBox({ type: 'info', message: 'Downloading ' + NAME + ' ' + v, detail: 'You can keep working. You will be asked to restart when it is ready.' });
    } else {
      if (!byHand && upd.told === v) return; upd.told = v;
      const asset = (rel.assets || []).find(a => IS_MAC && a.name.startsWith(NAME.replace(' ', '-') + '-') && (process.arch === 'arm64' ? /Mac-Apple/i : /Mac-Intel/i).test(a.name) && /\.dmg$/i.test(a.name));
      const r = await dialog.showMessageBox({ type: 'info', buttons: ['Download', 'Later'], defaultId: 0, cancelId: 1, message: 'New version — ' + NAME + ' ' + v, detail: 'Download it, close ' + NAME + ', then drag the new ' + NAME + ' into Applications (replace the old one).' });
      if (r.response === 0) shell.openExternal(asset ? asset.browser_download_url : rel.html_url);
    }
  } catch (e) { log('Update check failed', e && e.message); }
  finally { upd.busy = false; }
}

/* ---------- start ---------- */
ipcMain.on('lucon-info', e => { e.returnValue = { app: true, flavour: FLAVOUR, name: NAME, version: app.getVersion(), platform: process.platform, arch: process.arch }; });
app.on('second-instance', () => { if (win) { if (win.isMinimized()) win.restore(); win.show(); win.focus(); } else createWindow(); });
app.whenReady().then(async () => {
  loadSet(); log(NAME, app.getVersion(), process.platform, process.arch, 'Electron', process.versions.electron);
  wirePermissions(session.defaultSession);
  if (IS_MAC && !SMOKE) { for (const m of ['camera', 'microphone']) { try { if (systemPreferences.getMediaAccessStatus(m) === 'not-determined') await systemPreferences.askForMediaAccess(m); } catch {} } }
  startLink();
  buildMenu();
  if (SMOKE) return smokeTest();
  createWindow();
  setTimeout(pollLink, 1500); setInterval(pollLink, 5000);
  setTimeout(() => checkUpdates(false), 20000); setInterval(() => checkUpdates(false), 6 * 3600 * 1000);
});
app.on('activate', () => { if (!win) createWindow(); else win.show(); });
app.on('window-all-closed', () => { if (!IS_MAC || SMOKE) app.quit(); });
app.on('before-quit', () => { quitting = true; });
app.on('will-quit', () => { stopLink(); });

/* ---------- automatic test (used by the build on GitHub) ---------- */
async function smokeTest() {
  const out = { ok: false, steps: {} };
  const done = (ok) => { out.ok = ok; try { fs.writeFileSync(SMOKE, JSON.stringify(out, null, 2)); } catch {} stopLink(); setTimeout(() => app.exit(ok ? 0 : 1), 2000); };
  setTimeout(() => { out.steps.timeout = true; done(false); }, 90000);
  try {
    for (let i = 0; i < 40 && !linkState; i++) { await new Promise(r => setTimeout(r, 500)); linkState = await linkGet('/api/state'); }
    out.steps.link = linkState ? { version: linkState.version, whisper: linkState.whisper, features: linkState.features } : null;
    // the test page is the website's home page; its security rules do not list Lucon Link, so only for this test they are relaxed
    session.defaultSession.webRequest.onHeadersReceived((d, cb) => { const h = d.responseHeaders || {}; for (const k of Object.keys(h)) if (/content-security-policy/i.test(k)) delete h[k]; cb({ responseHeaders: h }); });
    const w = createWindow(SITE + '/');
    await new Promise(r => w.webContents.once('did-finish-load', r));
    out.steps.title = w.getTitle();
    out.steps.flavour = FLAVOUR; out.steps.page = await w.webContents.executeJavaScript(`(async()=>{ const r={app:!!(window.luconApp&&window.luconApp.app),version:window.luconApp&&window.luconApp.version,flavour:window.luconApp&&window.luconApp.flavour};
      try{ const j=await (await fetch('${LINK}/api/whisper',{cache:'no-store'})).json(); r.whisper=j.state; r.model=j.model; }catch(e){ r.whisperErr=String(e); }
      try{ const s=await navigator.mediaDevices.enumerateDevices(); r.devices=s.length; }catch(e){ r.devErr=String(e); }
      r.canvas=!!document.createElement('canvas').getContext('2d'); r.speechApi=!!(window.SpeechRecognition||window.webkitSpeechRecognition); return r; })()`);
    if (process.env.LUCON_SMOKE_PCM && fs.existsSync(process.env.LUCON_SMOKE_PCM)) {
      const pcm = fs.readFileSync(process.env.LUCON_SMOKE_PCM).toString('base64');
      out.steps.whisper = await w.webContents.executeJavaScript(`(async()=>{ const b=Uint8Array.from(atob('${pcm}'),c=>c.charCodeAt(0)); const t0=performance.now();
        const r=await fetch('${LINK}/api/whisper?lang=en',{method:'POST',headers:{'Content-Type':'application/octet-stream'},body:b.buffer}); const j=await r.json(); j.status=r.status; j.wall=Math.round(performance.now()-t0); return j; })()`);
    }
    const p = out.steps.page || {};
    const wok = !process.env.LUCON_SMOKE_PCM || /faith/i.test((out.steps.whisper || {}).text || '');
    done(!!(p.app && p.flavour === FLAVOUR && out.steps.link && (p.whisper === 'idle' || p.whisper === 'ready' || p.whisper === 'starting') && wok));
  } catch (e) { out.steps.error = String(e && e.stack || e); done(false); }
}
