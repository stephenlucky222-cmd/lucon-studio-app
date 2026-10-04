const { contextBridge, ipcRenderer } = require('electron');
contextBridge.exposeInMainWorld('picker', {
  onList: f => ipcRenderer.on('picker-list', (e, list, win) => f(list, win)),
  done: (id, audio) => ipcRenderer.send('picker-done', id, !!audio)
});
