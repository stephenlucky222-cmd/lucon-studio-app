// Chooses which app the next build makes: "studio" (Lucon Studio) or "events" (Lucon Events).
// Writes flavour.json (read by main.js) and dist-config.json (the electron-builder settings for that app).
'use strict';
const fs = require('fs');
const path = require('path');
const id = process.argv[2] === 'events' ? 'events' : 'studio';
const root = path.join(__dirname, '..');
const pkg = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
const c = JSON.parse(JSON.stringify(pkg.build));
fs.writeFileSync(path.join(root, 'flavour.json'), JSON.stringify({ id }, null, 2) + '\n');
if (id === 'events') {
  const N = 'Lucon Events';
  c.appId = 'com.luconhouse.events';
  c.productName = N;
  c.extraMetadata = { name: 'lucon-events', productName: N, description: 'Lucon Events: registration, check-in, badges, reports and Lucon Studio.' };
  c.directories = Object.assign({}, c.directories, { output: 'dist-events' });
  c.mac = Object.assign({}, c.mac, { icon: 'build/events/icon.png', category: 'public.app-category.business' });
  c.mac.extendInfo = Object.assign({}, c.mac.extendInfo, {
    NSCameraUsageDescription: 'Lucon Events uses the camera to scan tickets and badges, and for Lucon Studio.',
    NSMicrophoneUsageDescription: 'Lucon Events uses microphones in Lucon Studio for the programme sound and live captions.',
    NSAudioCaptureUsageDescription: 'Lucon Events can take the computer\'s sound when you share a screen in Lucon Studio.',
    LSApplicationCategoryType: 'public.app-category.business' });
  c.dmg = Object.assign({}, c.dmg, { title: N });
  c.win = Object.assign({}, c.win, { icon: 'build/events/icon.ico' });
  c.nsis = Object.assign({}, c.nsis, { shortcutName: N });
  // its own update file (events.yml) so the two apps never pick up each other's updates
  c.publish = c.publish.map(p => Object.assign({}, p, { channel: 'events' }));
}
// the download's file name, e.g. ARTIFACT=Mac-Apple.${ext} -> Lucon-Events-Mac-Apple.dmg
if (process.env.ARTIFACT) c.artifactName = (id === 'events' ? 'Lucon-Events-' : 'Lucon-Studio-') + process.env.ARTIFACT;
fs.writeFileSync(path.join(root, 'dist-config.json'), JSON.stringify(c, null, 2) + '\n');
console.log('Building', id === 'events' ? 'Lucon Events' : 'Lucon Studio', '->', c.directories.output);
