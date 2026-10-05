// Tells the website it is running inside the Lucon Events or Lucon Studio app (so it uses the built-in offline captions).
const { contextBridge, ipcRenderer } = require('electron');
let info = { app: true };
try { info = ipcRenderer.sendSync('lucon-info') || info; } catch {}
contextBridge.exposeInMainWorld('luconApp', Object.freeze({ app: true, flavour: info.flavour || 'studio', name: info.name || 'Lucon Studio', version: info.version, platform: info.platform, arch: info.arch }));
