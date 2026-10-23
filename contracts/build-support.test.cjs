'use strict';
const assert = require('node:assert/strict');
const { test } = require('node:test');
const crypto = require('node:crypto');
const zlib = require('node:zlib');
const { BuildRefusal, parseJSON, imports, sourceClosure, tarSources, dependencySources, sha256, canonical } = require('./build-support.cjs');

// Independent small USTAR fixtures; all adversarial cases have valid header
// checksums unless checksum failure is the boundary being tested. No files are
// extracted or written and production storage/decompression floors are retained.
function archive(members, terminate = true) {
  const result = [];
  for (const member of members) {
    const body = Buffer.from(member.body ?? ''); const header = Buffer.alloc(512);
    header.write(member.name, 0, 100, 'ascii'); header.write('0000644\0', 100, 8, 'ascii');
    header.write('0000000\0', 108, 8, 'ascii'); header.write('0000000\0', 116, 8, 'ascii');
    header.write((member.size ?? body.length).toString(8).padStart(11, '0') + '\0', 124, 12, 'ascii');
    header.write('00000000000\0', 136, 12, 'ascii'); header.fill(32, 148, 156);
    header[156] = (member.type ?? '0').charCodeAt(0); if (member.link) header.write(member.link, 157, 100, 'ascii');
    Buffer.from([117, 115, 116, 97, 114, 0, 48, 48]).copy(header, 257);
    if (member.prefix) header.write(member.prefix, 345, 155, 'ascii');
    const checksum = header.reduce((sum, byte) => sum + byte, 0);
    header.write(checksum.toString(8).padStart(6, '0') + '\0 ', 148, 8, 'ascii');
    result.push(header, body, Buffer.alloc((512 - body.length % 512) % 512));
  }
  if (terminate) result.push(Buffer.alloc(1024)); return Buffer.concat(result);
}
function refuses(run) { assert.throws(run, BuildRefusal); }

test('JSON validates hidden duplicates, bounds, encoding and exact numeric values', () => {
  assert.equal(canonical(parseJSON(Buffer.from('{"b":[1,true,null],"a":"\\uD83D\\uDC4D"}'))), '{"a":"👍","b":[1,true,null]}');
  for (const value of [
    '{"ignored":{"key":1,"key":2}}', '{"key":1,"\\u006bey":2}', '{"a":0}{}',
    '[01]', '[NaN]', '[1.5]', '[9007199254740993]', '{"a":"\\ud800"}', '{"a":[}',
    '"unterminated', '['.repeat(66) + '0' + ']'.repeat(66),
  ]) refuses(() => parseJSON(Buffer.from(value)));
  refuses(() => parseJSON(Buffer.from([0xff])));
  refuses(() => parseJSON(Buffer.from('{}'), 1));
  assert.equal(Object.getPrototypeOf(parseJSON(Buffer.from('{"__proto__":{"x":1}}'))), null);
  assert.equal({}.x, undefined);
});

test('literal import lexer ignores comments and quoted examples, including re-exports', () => {
  const source = `// import "private.sol";
    /* import {Bad} from 'unwanted.sol'; */
    contract Example { string public label = "import 'not-a-unit.sol';"; }
    import { ERC20 as Token } from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
    import /* comment */ './Compatibility.sol' as Compatible;
    import * as Types from "../interfaces/Types.sol";
  `;
  assert.deepEqual(imports(source), ['@openzeppelin/contracts/token/ERC20/ERC20.sol', './Compatibility.sol', '../interfaces/Types.sol']);
  for (const source of ['import "https://source.fixture.invalid/a.sol";', 'import "../a\\x2esol";', 'import "A.sol" "B.sol";', 'import "A.sol"', '/* not terminated', 'contract A { string s = "not terminated; }']) refuses(() => imports(source));
});

test('closure includes import-only compatibility files and preserves cycles without callbacks', () => {
  const units = new Map([
    ['apps/Main.sol', Buffer.from('import {Token} from "@openzeppelin/contracts/interfaces/Token.sol"; contract Main {}')],
    ['@openzeppelin/contracts/interfaces/Token.sol', Buffer.from('import {Token} from "../token/Token.sol";')],
    ['@openzeppelin/contracts/token/Token.sol', Buffer.from('import "../interfaces/Token.sol"; contract Token {}')],
  ]);
  const result = sourceClosure(['apps/Main.sol'], unit => units.get(unit));
  assert.deepEqual(Object.keys(result), [...units.keys()].sort());
  const escaping = new Map([['@openzeppelin/contracts/A.sol', Buffer.from('import "../../../private.sol";')]]);
  refuses(() => sourceClosure(['@openzeppelin/contracts/A.sol'], unit => escaping.get(unit)));
  refuses(() => sourceClosure(['../private.sol'], () => Buffer.from('contract A {}')));
  refuses(() => sourceClosure(['apps/Main.sol'], () => Buffer.alloc((1 << 20) + 1)));
});

test('USTAR reader validates the complete archive while selecting only source/identity bytes', () => {
  const raw = archive([{ name: 'package/A.sol', body: 'contract A {}' }, { name: 'package/build/A.json', body: '{}' }, { name: 'package/package.json', body: '{"name":"@openzeppelin/contracts","version":"5.6.1"}' }]);
  const result = tarSources(raw);
  assert.equal(result.members, 3); assert.deepEqual([...result.files.keys()], ['package/A.sol', 'package/package.json']);
  assert.equal(result.files.get('package/A.sol').toString(), 'contract A {}');
});

test('USTAR refuses links, traversal, extensions, case aliases and undeclared bytes', () => {
  for (const members of [
    [{ name: 'package/../outside.sol' }], [{ name: '/outside.sol' }], [{ name: 'package/./A.sol' }],
    [{ name: 'package/A.sol', type: '2', link: '../outside' }], [{ name: 'package/A.sol', type: '1', link: 'other' }],
    [{ name: 'package/A.sol', type: 'x' }], [{ name: 'package/A.sol', type: 'L' }],
    [{ name: 'package/A.sol' }, { name: 'package/a.sol' }], [{ name: 'package/A.sol', size: (8 << 20) + 1 }],
    [{ name: 'A.sol', prefix: 'package/..' }],
  ]) refuses(() => tarSources(archive(members)));
  const checksum = archive([{ name: 'package/A.sol' }]); checksum[0] ^= 1; refuses(() => tarSources(checksum));
  refuses(() => tarSources(archive([{ name: 'package/A.sol' }], false)));
  const trailing = archive([{ name: 'package/A.sol' }]); trailing[trailing.length - 1] = 1; refuses(() => tarSources(trailing));
  const padding = archive([{ name: 'package/A.sol', body: 'x' }]); padding[513] = 1; refuses(() => tarSources(padding));
  refuses(() => tarSources(archive([{ name: 'package/A.sol' }, { name: 'package/B.sol' }]), { maxMembers: 1 }));
  refuses(() => tarSources(archive([{ name: 'package/A.sol' }]), { maxBytes: 64 << 20 | 1 }));
});

test('USTAR accepts an explicit empty package root while refusing root aliases and payloads', () => {
  for (const name of ['package', 'package/']) {
    const result = tarSources(archive([{ name, type: '5' }, { name: 'package/A.sol', body: 'contract A {}' }]));
    assert.equal(result.members, 2); assert.equal(result.declaredBytes, 13);
    assert.deepEqual([...result.files.keys()], ['package/A.sol']);
  }
  for (const members of [
    [{ name: 'package', body: 'not a directory' }],
    [{ name: 'package/', type: '5', body: 'hidden data' }],
    [{ name: 'package/', type: '2', link: 'outside' }],
    [{ name: 'package/', type: '1', link: 'outside' }],
    [{ name: './package/', type: '5' }],
    [{ name: 'Package/', type: '5' }],
    [{ name: 'package/', type: '5' }, { name: 'package', type: '5' }],
    [{ name: 'package/', type: '5' }, { name: 'package/', type: '5' }],
    [{ name: '', prefix: 'package', type: '5' }],
  ]) refuses(() => tarSources(archive(members)));
});

test('dependency integrity precedes bounded in-memory decompression and package identity', () => {
  const raw = zlib.gzipSync(archive([{ name: 'package/A.sol', body: 'contract A {}' }, { name: 'package/package.json', body: '{"name":"@openzeppelin/contracts","version":"5.6.1"}' }]));
  const pin = { bytes: raw.length, sha256: sha256(raw), integrity: 'sha512-' + crypto.createHash('sha512').update(raw).digest('base64') };
  assert.equal(dependencySources(raw, pin).sources.get('@openzeppelin/contracts/A.sol').toString(), 'contract A {}');
  refuses(() => dependencySources(raw, { ...pin, integrity: 'sha512-invalid' }));
  refuses(() => dependencySources(raw, pin, { maxUnpackedBytes: 1024 }));
  refuses(() => dependencySources(raw, pin, { maxUnpackedBytes: (64 << 20) + 1 }));
});
