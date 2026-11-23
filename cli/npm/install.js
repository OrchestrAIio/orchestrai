#!/usr/bin/env node
'use strict';

// postinstall: fetch the verified prebuilt binary. A failure here does not
// fail `npm install`; the launcher retries the download on first run.

const { install } = require('./lib/binary');

if (process.env.ORCHESTRAI_CLI_SKIP_DOWNLOAD || process.env.ORCHESTRAI_CLI_BINARY_PATH) {
  process.exit(0);
}

install({ log: msg => process.stderr.write(`@orchestrai/cli: ${msg}\n`) })
  .then(dest => {
    process.stderr.write(`@orchestrai/cli: installed ${dest}\n`);
  })
  .catch(err => {
    process.stderr.write(
      `@orchestrai/cli: could not download the binary now (${err.message}); ` +
        'it will be fetched on first run.\n'
    );
  });
