'use strict';

// Resolves, downloads and verifies the prebuilt `orchestrai` binary that the
// GitHub release for this package version ships.

const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

const pkg = require('../package.json');

const REPO = 'OrchestrAIio/orchestrai';
const TAG_PREFIX = 'ork-v';

const PLATFORMS = { linux: 'linux', darwin: 'darwin', win32: 'windows' };
const ARCHES = { x64: 'amd64', arm64: 'arm64' };

function target(platform = process.platform, arch = process.arch) {
  const os = PLATFORMS[platform];
  const cpu = ARCHES[arch];
  if (!os || !cpu) {
    throw new Error(
      `@orchestrai/cli has no prebuilt binary for ${platform}/${arch}. ` +
        'Supported: linux, macOS and Windows on x64 or arm64.'
    );
  }
  return { os, cpu, exe: os === 'windows' ? '.exe' : '' };
}

function assetName(version, platform, arch) {
  const t = target(platform, arch);
  return `orchestrai_${version}_${t.os}_${t.cpu}${t.exe}`;
}

function checksumsName(version) {
  return `orchestrai_${version}_checksums.txt`;
}

function downloadBase(version) {
  const override = process.env.ORCHESTRAI_CLI_DOWNLOAD_BASE;
  if (override) return override.replace(/\/+$/, '');
  return `https://github.com/${REPO}/releases/download/${TAG_PREFIX}${version}`;
}

// parseChecksums reads `sha256sum` output: "<hex>  <file>" per line.
function parseChecksums(text) {
  const sums = new Map();
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line) continue;
    const m = /^([a-f0-9]{64})\s+\*?(.+)$/i.exec(line);
    if (!m) throw new Error(`malformed checksums line: ${line}`);
    sums.set(m[2].trim(), m[1].toLowerCase());
  }
  return sums;
}

function sha256(buf) {
  return crypto.createHash('sha256').update(buf).digest('hex');
}

function vendorDir() {
  return path.join(__dirname, '..', 'vendor');
}

function binaryPath(platform = process.platform) {
  if (process.env.ORCHESTRAI_CLI_BINARY_PATH) {
    return process.env.ORCHESTRAI_CLI_BINARY_PATH;
  }
  return path.join(
    vendorDir(),
    platform === 'win32' ? 'orchestrai.exe' : 'orchestrai'
  );
}

async function fetchBuffer(url) {
  const res = await fetch(url, { redirect: 'follow' });
  if (!res.ok) {
    throw new Error(`GET ${url} failed: HTTP ${res.status}`);
  }
  return Buffer.from(await res.arrayBuffer());
}

// install downloads the binary for this platform, verifies its sha256
// against the release checksums file, and installs it atomically.
async function install({ version = pkg.version, log = () => {} } = {}) {
  const asset = assetName(version);
  const base = downloadBase(version);
  log(`Downloading ${asset}…`);
  const [sumsText, bin] = await Promise.all([
    fetchBuffer(`${base}/${checksumsName(version)}`).then(b => b.toString('utf8')),
    fetchBuffer(`${base}/${asset}`),
  ]);
  const expected = parseChecksums(sumsText).get(asset);
  if (!expected) {
    throw new Error(`${checksumsName(version)} has no entry for ${asset}`);
  }
  const actual = sha256(bin);
  if (actual !== expected) {
    throw new Error(
      `checksum mismatch for ${asset}: expected ${expected}, got ${actual}. Refusing to install.`
    );
  }
  const dest = binaryPath();
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  const tmp = `${dest}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, bin, { mode: 0o755 });
  fs.renameSync(tmp, dest);
  return dest;
}

/**
 * How the user ran the CLI, for the hints it prints: "npx @orchestrai/cli"
 * when it runs through npx / npm exec (the bin is not on PATH then), else the
 * bin name that was invoked ("ork" or "orchestrai").
 */
function invokedAs(env = process.env, scriptPath = process.argv[1] || '') {
  const npx =
    env.npm_command === 'exec' ||
    env.npm_lifecycle_event === 'npx' ||
    /[\\/]_npx[\\/]/.test(scriptPath);
  if (npx) {
    return 'npx @orchestrai/cli';
  }
  return path.basename(scriptPath).replace(/\.js$/, '') === 'ork' ? 'ork' : 'orchestrai';
}

module.exports = {
  assetName,
  invokedAs,
  binaryPath,
  checksumsName,
  downloadBase,
  install,
  parseChecksums,
  sha256,
  target,
};
