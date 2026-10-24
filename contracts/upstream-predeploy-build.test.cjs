'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const { BuildRefusal, canonical, sha256, parseJSON } = require('./build-support.cjs');
const { COMPILERS, loadUpstreamCompiler } = require('./upstream-solc.cjs');
const { validateLock, sourcesFor, compareRuntime, materialFor, reconstruct } = require('./rebuild-upstream-predeploys.cjs');
const lock = parseJSON(fs.readFileSync(path.join(__dirname, 'upstream-predeploy-build.lock.json')), 256 << 10);
const refusal = action => assert.throws(action, BuildRefusal);

test('reviewed source lock preserves five compiler groups and all eight targets', () => {
  const checked = validateLock(structuredClone(lock));
  assert.deepEqual(checked.groups.map(g => g.name), ['multicall', 'op', 'permit2', 'entrypoint', 'factory']);
  assert.equal(checked.groups.reduce((n, g) => n + g.targets.length, 0), 8);
  assert.equal(checked.groups.reduce((n, g) => n + g.sources.length, 0), 50);
  assert.equal(checked.groups.find(g => g.name === 'multicall').sources[0].unit, 'Multicall3.sol');
});
test('lock refuses missing, duplicate, reordered and redirected source or target inventory', () => {
  for (const change of [v => v.groups.pop(), v => v.groups.reverse(), v => { v.groups[1] = v.groups[0] },
    v => v.groups[1].targets.pop(), v => v.groups[1].sources.push(v.groups[1].sources[0]),
    v => { v.groups[0].targets[0].address = '0x' + '00'.repeat(20) },
    v => { v.groups[0].sources[0].origin.path = '../source.sol' },
    v => { v.groups[0].sources[0].origin.blob = 'unknown' },
    v => { v.groups[0].compiler.sha256 = '0'.repeat(64) },
    v => { v.groups[0].compilerSource.path = 'unreviewed.js' },
    v => { v.groups[0].targets[0].coreRuntime = '../other.hex' },
    v => { v.groups[2].targets[0].historicalContext.chainId = 7340470 },
    v => { v.groups[3].targets[0].historicalContext.childNonce = 0 },
    v => { v.groups[0].sources[0].extra = true }]) {
    const value = structuredClone(lock); change(value); refusal(() => validateLock(value));
  }
});
test('all compiler pins are finite and unreviewed modules are refused before loading', () => {
  for (const [group, pin] of Object.entries(COMPILERS)) {
    refusal(() => loadUpstreamCompiler('untrusted-module.js', group, { ...pin, sha256: '0'.repeat(64) }));
    refusal(() => loadUpstreamCompiler('untrusted-module.js', group, { ...pin, arguments: 1 }));
  }
  refusal(() => loadUpstreamCompiler('untrusted-module.js', 'other', COMPILERS.multicall));
});
function source(unit, content) {
  const raw = Buffer.from(content);
  return { unit, bytes: raw.length, sha256: sha256(raw), origin: { kind: 'git', repository: 'example/source', commit: '1'.repeat(40), path: unit,
    blob: crypto.createHash('sha1').update(Buffer.from(`blob ${raw.length}\0`)).update(raw).digest('hex') }, equivalentOrigins: [] };
}
function fixture(contents = { 'src/Target.sol': 'import {Helper} from "solmate/src/Helper.sol"; contract Target {}',
  'lib/solmate/src/Helper.sol': 'contract Helper {}' }) {
  const group = { name: 'permit2', language: 'Solidity', sources: Object.entries(contents).map(([u, c]) => source(u, c)), targets: [{ unit: 'src/Target.sol' }] };
  const bundle = { schema: 1, group: 'permit2', sources: Object.fromEntries(Object.entries(contents).map(([unit, content]) => [unit, { content }])) };
  return { group, bundle };
}
test('literal sources resolve the declared Solmate remapping without callbacks', () => {
  const { group, bundle } = fixture();
  assert.deepEqual(Object.keys(sourcesFor(bundle, group)), ['lib/solmate/src/Helper.sol', 'src/Target.sol']);
});
test('literal sources refuse changed bytes, duplicate JSON keys, extra units and URL loaders', () => {
  for (const change of [b => { b.sources['src/Target.sol'].content += '\n' },
    b => { b.sources.extra = { content: 'contract Extra {}' } },
    b => { delete b.sources['lib/solmate/src/Helper.sol'] },
    b => { b.sources['src/Target.sol'] = { urls: ['https://example.invalid/source.sol'] } },
    b => { b.group = 'entrypoint' }]) {
    const { group, bundle } = fixture(); change(bundle); refusal(() => sourcesFor(bundle, group));
  }
  refusal(() => parseJSON(Buffer.from('{"schema":1,"group":"permit2","sources":{"a":{"content":"x","content":"y"}}}')));
});
test('matching source hashes do not bypass missing imports, escape or unused source checks', () => {
  for (const contents of [
    { 'src/Target.sol': 'import "./Missing.sol"; contract Target {}' },
    { 'src/Target.sol': 'import "../../outside.sol"; contract Target {}' },
    { 'src/Target.sol': 'contract Target {}', 'src/Unused.sol': 'contract Unused {}' },
    { 'src/Target.sol': 'import "https://example.invalid/source.sol"; contract Target {}' },
  ]) {
    const { group, bundle } = fixture(contents); refusal(() => sourcesFor(bundle, group));
  }
  const { group, bundle } = fixture(); group.sources[0].origin.blob = '0'.repeat(40); refusal(() => sourcesFor(bundle, group));
});
function immutableFixture() {
  const template = Buffer.alloc(110, 0x11); template.fill(0, 5, 37); template.fill(0, 45, 77);
  const word = '22'.repeat(32); const expected = Buffer.from(template);
  expected.write(word, 5, 'hex'); expected.write(word, 45, 'hex');
  return { template, expected, references: { 140: [{ start: 5, length: 32 }, { start: 45, length: 32 }] }, words: { 140: word } };
}
test('immutable comparison accounts for every zero-template range and preserves all other bytes', () => {
  const v = immutableFixture();
  assert.deepEqual(compareRuntime(v.template, v.expected, v.references, v.words), { fullRuntimeEqual: false, immutableRanges: 2, metadataStripped: false });
  assert.equal(compareRuntime(Buffer.from('aabb', 'hex'), Buffer.from('aabb', 'hex'), {}, {}).fullRuntimeEqual, true);
  v.expected[v.expected.length - 1] ^= 1;
  refusal(() => compareRuntime(v.template, v.expected, v.references, v.words));
});
test('immutable comparison refuses overlap, nonzero templates and mismatched historical words', () => {
  for (const change of [v => { v.references[140][1].start = 6 }, v => { v.references[140][0].start = -1 },
    v => { v.references[140][0].length = 31 }, v => { v.references[140][0].start = 100 },
    v => { v.references[140] = [] }, v => { v.template[5] = 1 }, v => { v.expected[45] ^= 1 },
    v => { v.words[140] = '33'.repeat(32) }, v => { v.words.extra = '22'.repeat(32) }]) {
    const v = immutableFixture(); change(v); refusal(() => compareRuntime(v.template, v.expected, v.references, v.words));
  }
});
function yulFixture() {
  const runtime = Buffer.alloc(69, 0x11); const creation = Buffer.concat([Buffer.from('604580600e600039806000f350fe', 'hex'), runtime]);
  const artifact = { evm: { bytecode: { object: creation.toString('hex'), linkReferences: {} } } };
  const target = { creationBytes: creation.length, creationSha256: sha256(creation), runtimeTemplateBytes: runtime.length,
    runtimeTemplateSha256: sha256(runtime), abiSha256: sha256(canonical(null)), metadataSha256: sha256(''),
    storageLayoutSha256: sha256(canonical(null)), immutableReferences: {} };
  return { artifact, target, runtime };
}
test('legacy Yul creation is retained without inventing a Solidity ABI, layout or metadata', () => {
  const { artifact, target, runtime } = yulFixture();
  assert.deepEqual(materialFor(artifact, target, true).runtime, runtime);
  refusal(() => materialFor(artifact, target, false));
  artifact.metadata = '{}'; refusal(() => materialFor(artifact, target, true));
});
test('Yul copy-prefix and all creation bytes remain exact and library links are refused', () => {
  for (const change of [v => { v.artifact.evm.bytecode.object = '61' + v.artifact.evm.bytecode.object.slice(2) },
    v => { v.artifact.evm.bytecode.object += '00' }, v => { v.artifact.evm.bytecode.linkReferences = { unit: {} } },
    v => { v.target.creationSha256 = '0'.repeat(64) }]) {
    const value = yulFixture(); change(value); refusal(() => materialFor(value.artifact, value.target, true));
  }
});
test('CLI requires one named group and refuses unknown, repeated or incomplete flags', () => {
  for (const args of [[], ['--group', 'other'], ['--emit-bundle', '--emit-bundle'], ['--unknown', 'value'],
    ['--compiler'], ['--group', 'op', '--group', 'op'], ['--group', 'op', '--compiler', 'value', '--source-bundle', 'value']]) refusal(() => reconstruct(args));
});
