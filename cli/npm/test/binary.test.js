'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { test } = require('node:test');

const binary = require('../lib/binary');

test('asset names cover every release target', () => {
  assert.equal(binary.assetName('1.2.3', 'linux', 'x64'), 'orchestrai_1.2.3_linux_amd64');
  assert.equal(binary.assetName('1.2.3', 'darwin', 'arm64'), 'orchestrai_1.2.3_darwin_arm64');
  assert.equal(binary.assetName('1.2.3', 'win32', 'x64'), 'orchestrai_1.2.3_windows_amd64.exe');
  assert.throws(() => binary.assetName('1.2.3', 'aix', 'ppc64'), /no prebuilt binary/);
});

test('parseChecksums reads sha256sum output', () => {
  const sums = binary.parseChecksums(
    'ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789  orchestrai_1_linux_amd64\n' +
      'abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789 *orchestrai_1_windows_amd64.exe\r\n'
  );
  assert.equal(
    sums.get('orchestrai_1_linux_amd64'),
    'abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789'
  );
  assert.ok(sums.has('orchestrai_1_windows_amd64.exe'));
  assert.throws(() => binary.parseChecksums('nope'), /malformed/);
});

function serve(files) {
  return new Promise(resolve => {
    const srv = http.createServer((req, res) => {
      const name = decodeURIComponent(req.url.split('/').pop());
      if (!(name in files)) {
        res.writeHead(404).end();
        return;
      }
      res.writeHead(200).end(files[name]);
    });
    srv.listen(0, '127.0.0.1', () => resolve(srv));
  });
}

test('install verifies the checksum before writing', async t => {
  const version = '9.9.9';
  const asset = binary.assetName(version);
  const payload = Buffer.from('#!/bin/sh\necho ok\n');
  const files = {
    [asset]: payload,
    [binary.checksumsName(version)]: `${binary.sha256(payload)}  ${asset}\n`,
  };
  const srv = await serve(files);
  t.after(() => srv.close());
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'orchestrai-npm-'));
  process.env.ORCHESTRAI_CLI_DOWNLOAD_BASE = `http://127.0.0.1:${srv.address().port}/dl/`;
  process.env.ORCHESTRAI_CLI_BINARY_PATH = path.join(dir, 'orchestrai');
  t.after(() => {
    delete process.env.ORCHESTRAI_CLI_DOWNLOAD_BASE;
    delete process.env.ORCHESTRAI_CLI_BINARY_PATH;
    fs.rmSync(dir, { recursive: true, force: true });
  });

  const dest = await binary.install({ version });
  assert.deepEqual(fs.readFileSync(dest), payload);
  if (process.platform !== 'win32') {
    assert.equal(fs.statSync(dest).mode & 0o777, 0o755);
  }

  fs.rmSync(dest);
  files[binary.checksumsName(version)] = `${'0'.repeat(64)}  ${asset}\n`;
  await assert.rejects(binary.install({ version }), /checksum mismatch/);
  assert.equal(fs.existsSync(dest), false);

  files[binary.checksumsName(version)] = `${'0'.repeat(64)}  other-file\n`;
  await assert.rejects(binary.install({ version }), /no entry/);
});

test('invokedAs names npx runs so hints stay copyable', () => {
  assert.equal(binary.invokedAs({ npm_command: 'exec' }, '/home/a/.npm/_npx/abc/node_modules/.bin/orchestrai'), 'npx @orchestrai/cli');
  assert.equal(binary.invokedAs({}, '/home/a/.npm/_npx/abc/node_modules/@orchestrai/cli/bin/orchestrai.js'), 'npx @orchestrai/cli');
  assert.equal(binary.invokedAs({}, 'C:\\Users\\a\\AppData\\Local\\npm-cache\\_npx\\abc\\node_modules\\.bin\\orchestrai'), 'npx @orchestrai/cli');
  assert.equal(binary.invokedAs({}, '/usr/local/bin/ork'), 'ork');
  assert.equal(binary.invokedAs({}, '/usr/local/bin/orchestrai'), 'orchestrai');
  assert.equal(binary.invokedAs({ npm_command: 'run-script' }, '/repo/node_modules/.bin/orchestrai'), 'orchestrai');
});
