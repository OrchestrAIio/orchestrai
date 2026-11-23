/**
 * Mirrors the ACP Registry CI checks (agentclientprotocol/registry
 * CONTRIBUTING.md + agent.schema.json) so a submission PR does not bounce.
 */
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, it } from 'node:test';
import { PACKAGE_NAME, PACKAGE_VERSION } from '../src/config.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const entryDir = path.join(root, 'registry', 'orchestrai');
const agent = JSON.parse(readFileSync(path.join(entryDir, 'agent.json'), 'utf8')) as Record<string, unknown>;
const pkg = JSON.parse(readFileSync(path.join(root, 'package.json'), 'utf8')) as { name: string; version: string; bin: Record<string, string> };

describe('ACP Registry entry', () => {
  it('has the required fields and a valid id matching its directory', () => {
    for (const key of ['id', 'name', 'version', 'description', 'distribution', 'license_url']) {
      assert.ok(agent[key], `missing ${key}`);
    }
    assert.match(String(agent.id), /^[a-z][a-z0-9-]*$/);
    assert.equal(agent.id, path.basename(entryDir));
    assert.match(String(agent.version), /^[0-9]+\.[0-9]+\.[0-9]+$/);
    const allowed = new Set(['id', 'name', 'version', 'description', 'repository', 'website', 'authors', 'license', 'license_url', 'icon', 'distribution', 'preview']);
    for (const key of Object.keys(agent)) assert.ok(allowed.has(key), `unknown field ${key}`);
  });

  it('pins the npm package version to the package being published (no @latest)', () => {
    assert.equal(pkg.name, PACKAGE_NAME);
    assert.equal(pkg.version, PACKAGE_VERSION);
    assert.equal(agent.version, pkg.version);
    const npx = (agent.distribution as { npx?: { package?: string } }).npx;
    assert.equal(npx?.package, `${pkg.name}@${pkg.version}`);
    assert.ok(!String(npx?.package).endsWith('@latest'));
    assert.ok(String(agent.license_url).includes(`@${pkg.version}/`));
    assert.equal(pkg.bin['orchestrai-acp'], 'dist/index.js');
  });

  it('ships a 16x16 monochrome currentColor icon', () => {
    const svg = readFileSync(path.join(entryDir, 'icon.svg'), 'utf8');
    assert.match(svg, /width="16"/);
    assert.match(svg, /height="16"/);
    assert.match(svg, /viewBox="0 0 16 16"/);
    for (const m of svg.matchAll(/(fill|stroke)="([^"]*)"/g)) {
      assert.ok(['currentColor', 'none', 'inherit'].includes(m[2] ?? ''), `${m[1]}="${m[2]}" is not allowed`);
    }
  });
});
