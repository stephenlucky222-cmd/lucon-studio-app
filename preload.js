// Tells the Studio page it is running inside the Lucon Studio app (so it uses the built-in offline captions).
const { contextBridge, ipcRenderer } = require('electron');
let info = { app: true };
try { info = ipcRenderer.sendSync('lucon-info') || info; } catch {}
contextBridge.exposeInMainWorld('luconApp', Object.freeze({ app: true, version: info.version, platform: info.platform, arch: info.arch }));
