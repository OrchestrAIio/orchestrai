#!/usr/bin/env node
'use strict';

// Launcher for both `orchestrai` and `ork`: runs the native binary with the
// invoked name as argv[0], and tells it when it runs under npx, so help text
// and hints match what the user typed.

const fs = require('node:fs');
const { spawnSync } = require('node:child_process');
const { binaryPath, install, invokedAs } = require('../lib/binary');

async function main() {
  let bin = binaryPath();
  if (!fs.existsSync(bin)) {
    if (process.env.ORCHESTRAI_CLI_BINARY_PATH) {
      process.stderr.write(`orchestrai: ORCHESTRAI_CLI_BINARY_PATH=${bin} does not exist\n`);
      return 1;
    }
    try {
      bin = await install({ log: msg => process.stderr.write(`orchestrai: ${msg}\n`) });
    } catch (err) {
      process.stderr.write(`orchestrai: could not install the native binary: ${err.message}\n`);
      return 1;
    }
  }
  const prog = invokedAs(process.env, process.argv[1] || '');
  const argv0 = prog === 'ork' ? 'ork' : 'orchestrai';
  // The binary prints this name in every hint (`npx @orchestrai/cli init`).
  const env = { ...process.env, ORCHESTRAI_CLI_INVOKED_AS: prog };
  const res = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit', argv0, env });
  if (res.error) {
    process.stderr.write(`orchestrai: ${res.error.message}\n`);
    return 1;
  }
  if (res.signal) {
    process.kill(process.pid, res.signal);
    return 1;
  }
  return res.status ?? 1;
}

main().then(code => process.exit(code));
