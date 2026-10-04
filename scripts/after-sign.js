// Without an Apple Developer certificate the app is "ad-hoc" signed, so it can open on Apple-chip Macs
// (the first time: System Settings → Privacy & Security → Open Anyway). With a certificate, this step is skipped.
const { execFileSync } = require('child_process');
const path = require('path');
exports.default = async function (ctx) {
  if (ctx.electronPlatformName !== 'darwin') return;
  if (process.env.CSC_LINK || process.env.CSC_NAME) return;
  const app = path.join(ctx.appOutDir, ctx.packager.appInfo.productFilename + '.app');
  console.log('Ad-hoc signing', app);
  execFileSync('codesign', ['--force', '--deep', '--sign', '-', app], { stdio: 'inherit' });
  execFileSync('codesign', ['--verify', '--deep', '--strict', '--verbose=2', app], { stdio: 'inherit' });
};
