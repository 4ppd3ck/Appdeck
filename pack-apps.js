/*
 * pack-apps.js — build the shippable packages for the per-app distribution
 * model. Run from Launcher/:  node pack-apps.js [id]
 *
 *   dist/apps/<id>.zip          app files + app.json manifest at archive root
 *                               (exactly what POST /api/import expects)
 *   dist/appdeck-launcher.zip   the deck exe + empty Apps/ + starter apps.json
 *   dist/apps-catalog.json      simple catalog of the produced packages
 *
 * User data is never packed: *.db, data.js, config.json, logs, state.
 */
const fs = require('fs');
const path = require('path');
const { execSync } = require('child_process');

const ROOT = path.resolve(__dirname, '..');
const DIST = path.join(__dirname, 'dist');
const only = process.argv[2] || null;

const registry = JSON.parse(fs.readFileSync(path.join(__dirname, 'apps.json'), 'utf8'));
const EXCLUDE = [/\.db$/i, /\.sqlite$/i, /^data\.js$/i, /^config\.json$/i, /^state\.json$/i, /^logs$/i, /^test$/i];

function cpSync(src, dest) {
  fs.cpSync(src, dest, { recursive: true, filter: (s) => {
    const base = path.basename(s);
    return !EXCLUDE.some(re => re.test(base));
  }});
}

function zip(stageDir, outZip) {
  fs.rmSync(outZip, { force: true });
  // Compress-Archive includes the dir itself when given a dir; give it the
  // children so app.json lands at the archive root.
  const children = fs.readdirSync(stageDir).map(f => path.join(stageDir, f));
  execSync(`powershell -NoProfile -Command "Compress-Archive -Path '${children.join("','")}' -DestinationPath '${outZip}' -Force"`, { stdio: 'pipe' });
}

fs.rmSync(DIST, { recursive: true, force: true });
fs.mkdirSync(path.join(DIST, 'apps'), { recursive: true });

const apps = registry.apps.filter(a => a.dir && (!only || a.id === only));
if (!apps.length) { console.error('no matching app in apps.json'); process.exit(1); }

const catalog = [];
for (const entry of apps) {
  const src = path.resolve(__dirname, entry.dir);
  if (!fs.existsSync(src)) { console.error('SKIP (missing folder):', entry.id); continue; }
  const stage = path.join(DIST, '.stage', entry.id);
  cpSync(src, stage);

  const manifest = { ...entry };
  delete manifest.dir;
  fs.writeFileSync(path.join(stage, 'app.json'), JSON.stringify(manifest, null, 2));

  const out = path.join(DIST, 'apps', entry.id + '.zip');
  zip(stage, out);
  catalog.push({ id: entry.id, name: entry.name, desc: entry.desc, icon: entry.icon, category: entry.category, file: 'apps/' + entry.id + '.zip' });
  console.log('packed', entry.id, '->', 'dist/apps/' + entry.id + '.zip');
  fs.rmSync(stage, { recursive: true, force: true });
}

// launcher-only bundle: deck exe + empty Apps/ + starter registry
const exe = path.join(__dirname, process.platform === 'win32' ? 'appdeck.exe' : 'appdeck');
if (fs.existsSync(exe)) {
  const stage = path.join(DIST, '.stage', '_launcher');
  fs.mkdirSync(path.join(stage, 'Apps'), { recursive: true });
  fs.copyFileSync(exe, path.join(stage, path.basename(exe)));
  fs.writeFileSync(path.join(stage, 'apps.json'), JSON.stringify({ launcher_port: 9090, apps: [] }, null, 2));
  zip(stage, path.join(DIST, 'appdeck-launcher.zip'));
  fs.rmSync(stage, { recursive: true, force: true });
  console.log('packed launcher -> dist/appdeck-launcher.zip');
} else {
  console.log('launcher exe not found — run build.sh first');
}

fs.writeFileSync(path.join(DIST, 'apps-catalog.json'), JSON.stringify({ updated: new Date().toISOString(), apps: catalog }, null, 2));
console.log('catalog -> dist/apps-catalog.json');
