// Without an Apple Developer certificate the app is "ad-hoc" signed, so it can open on Apple-chip Macs
// (the first time: System Settings → Privacy & Security → Open Anyway). With a certificate, this step is skipped.
const { execFileSync } = require('child_process');
const path = require('path');
exports.default = async function (ctx) {
  if (ctx.electronPlatformName !== 'darwin') return;
  if (process.env.CSC_LINK || process.env.CSC_NAME) return;
  const app = path.join(ctx.appOutDir, ctx.packager.appInfo.productFilename + '.app');
  console.log('Ad-hoc signing', app);
  const fs = require('fs');
  const resDir = path.join(app, 'Contents', 'Resources');
  for (const sub of ['link', 'whisper']) {
    const d = path.join(resDir, sub); if (!fs.existsSync(d)) continue;
    for (const f of fs.readdirSync(d)) { const p = path.join(d, f); const st = fs.statSync(p); if (st.isFile() && (st.mode & 0o111) && !/\.(txt|bin)$/.test(f)) { console.log(' helper', f); execFileSync('codesign', ['--force', '--sign', '-', p], { stdio: 'inherit' }); } }
  }
  execFileSync('codesign', ['--force', '--deep', '--sign', '-', app], { stdio: 'inherit' });
  execFileSync('codesign', ['--verify', '--deep', '--strict', '--verbose=2', app], { stdio: 'inherit' });
};
