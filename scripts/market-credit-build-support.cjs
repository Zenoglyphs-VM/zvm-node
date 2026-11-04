'use strict';

const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const zlib = require('node:zlib');

class BuildRefusal extends Error {}
function check(value, message) { if (!value) throw new BuildRefusal(message); }
const sha256 = value => crypto.createHash('sha256').update(value).digest('hex');
function sort(value) {
  if (Array.isArray(value)) return value.map(sort);
  if (value && typeof value === 'object') return Object.fromEntries(Object.keys(value).sort().map(k => [k, sort(value[k])]));
  return value;
}
const canonical = value => JSON.stringify(sort(value));
function text(raw) {
  const value = raw.toString('utf8');
  check(Buffer.from(value).equals(raw), 'Invalid UTF-8 input');
  return value;
}
function readRegular(name, maximum) {
  const fd = fs.openSync(name, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
  try {
    const before = fs.fstatSync(fd);
    check(before.isFile() && before.size > 0 && before.size <= maximum, 'Input size or file type refused');
    const raw = fs.readFileSync(fd); const after = fs.fstatSync(fd);
    check(raw.length === before.size && after.size === before.size && after.mtimeMs === before.mtimeMs, 'Input changed during read');
    return raw;
  } finally { fs.closeSync(fd); }
}

// JSON.parse alone accepts duplicate keys and rounds large numeric literals.
// Bound and validate the complete document, including ignored nested values.
function parseJSON(raw, maximum = 8 << 20) {
  check(Buffer.isBuffer(raw) && raw.length > 0 && raw.length <= maximum, 'JSON byte bound refused');
  const source = text(raw); let cursor = 0; let remaining = 262144;
  const space = () => { while (/[ \t\r\n]/.test(source[cursor] ?? '\0')) ++cursor; };
  function string() {
    const start = cursor++;
    while (cursor < source.length) {
      const c = source[cursor++];
      if (c === '\\') { ++cursor; continue; }
      if (c !== '"') continue;
      let value;
      try { value = JSON.parse(source.slice(start, cursor)); }
      catch { throw new BuildRefusal('Invalid JSON string'); }
      check(Buffer.from(value).toString('utf8') === value, 'Unpaired JSON surrogate refused');
      return value;
    }
    throw new BuildRefusal('Unterminated JSON string');
  }
  function value(depth) {
    check(--remaining >= 0 && depth <= 64, 'JSON structure bound refused');
    space(); const c = source[cursor];
    if (c === '"') return string();
    if (c === '{') {
      ++cursor; space(); const result = Object.create(null);
      if (source[cursor] === '}') { ++cursor; return result; }
      while (true) {
        check(source[cursor] === '"', 'Invalid JSON object');
        const key = string(); check(!Object.hasOwn(result, key), 'Duplicate JSON object key');
        space(); check(source[cursor++] === ':', 'Invalid JSON member');
        result[key] = value(depth + 1); space();
        const end = source[cursor++]; if (end === '}') return result;
        check(end === ',', 'Invalid JSON object separator'); space();
      }
    }
    if (c === '[') {
      ++cursor; space(); const result = [];
      if (source[cursor] === ']') { ++cursor; return result; }
      while (true) {
        result.push(value(depth + 1)); space();
        const end = source[cursor++]; if (end === ']') return result;
        check(end === ',', 'Invalid JSON array separator');
      }
    }
    for (const [token, result] of [['true', true], ['false', false], ['null', null]]) {
      if (source.startsWith(token, cursor)) { cursor += token.length; return result; }
    }
    const number = source.slice(cursor).match(/^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/);
    check(number, 'Invalid JSON value'); cursor += number[0].length;
    const result = Number(number[0]); check(Number.isSafeInteger(result), 'Nonintegral or unsafe JSON number refused');
    return result;
  }
  const result = value(0); space(); check(cursor === source.length, 'Trailing JSON data');
  return result;
}

function sourceUnit(unit) {
  return typeof unit === 'string' && unit.length <= 512 && /^[A-Za-z0-9_@./-]+\.sol$/.test(unit)
    && !unit.startsWith('/') && path.posix.normalize(unit) === unit
    && unit.split('/').every(p => p !== '' && p !== '.' && p !== '..' && p !== '.git');
}
function imports(source) {
  const tokens = []; let cursor = 0;
  while (cursor < source.length) {
    const c = source[cursor];
    if (/\s/.test(c)) { ++cursor; continue; }
    if (source.startsWith('//', cursor)) {
      const end = source.indexOf('\n', cursor + 2); cursor = end < 0 ? source.length : end + 1; continue;
    }
    if (source.startsWith('/*', cursor)) {
      const end = source.indexOf('*/', cursor + 2); check(end >= 0, 'Unterminated Solidity comment'); cursor = end + 2; continue;
    }
    if (c === '"' || c === "'") {
      const start = cursor++; let ended = false;
      while (cursor < source.length) {
        const next = source[cursor++];
        if (next === '\\') { ++cursor; continue; }
        if (next === c) { ended = true; break; }
      }
      check(ended, 'Unterminated Solidity string'); tokens.push({ string: source.slice(start + 1, cursor - 1) }); continue;
    }
    const identifier = source.slice(cursor).match(/^[A-Za-z_$][A-Za-z0-9_$]*/);
    if (identifier) { tokens.push(identifier[0]); cursor += identifier[0].length; }
    else tokens.push(source[cursor++]);
  }
  const result = [];
  for (let i = 0; i < tokens.length; ++i) {
    if (tokens[i] !== 'import') continue;
    const paths = []; ++i;
    while (i < tokens.length && tokens[i] !== ';') {
      if (typeof tokens[i] === 'object') paths.push(tokens[i].string);
      ++i;
    }
    check(i < tokens.length && paths.length === 1 && /^[A-Za-z0-9_@./-]+$/.test(paths[0]), 'Only literal relative/package Solidity imports are accepted');
    result.push(paths[0]);
  }
  return result;
}
function sourceClosure(roots, loader) {
  const result = new Map(); const pending = [...roots]; let total = 0;
  while (pending.length) {
    const unit = pending.pop(); if (result.has(unit)) continue;
    check(sourceUnit(unit), 'Invalid Solidity source unit');
    const raw = loader(unit); check(Buffer.isBuffer(raw) && raw.length > 0 && raw.length <= 1 << 20, 'Source file byte bound refused');
    const content = text(raw); total += raw.length;
    check(total <= 16 << 20 && result.size < 1024, 'Source closure byte or member bound refused');
    result.set(unit, { content });
    for (const imported of imports(content)) {
      const target = imported.startsWith('.') ? path.posix.normalize(path.posix.join(path.posix.dirname(unit), imported)) : imported;
      check(sourceUnit(target) && (!unit.startsWith('@openzeppelin/contracts/') || target.startsWith('@openzeppelin/contracts/')),
        'Solidity import escapes its source package');
      pending.push(target);
    }
  }
  return Object.fromEntries([...result.entries()].sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0));
}

function tarSources(raw, options = {}) {
  const maxBytes = options.maxBytes ?? 64 << 20; const maxMembers = options.maxMembers ?? 4096;
  check(Number.isSafeInteger(maxBytes) && maxBytes > 0 && maxBytes <= 64 << 20 && Number.isSafeInteger(maxMembers) && maxMembers > 0 && maxMembers <= 4096, 'Invalid restricted archive bound');
  check(Buffer.isBuffer(raw) && raw.length > 0 && raw.length <= maxBytes && raw.length % 512 === 0, 'Archive size or block alignment refused');
  const zero = block => block.every(b => b === 0);
  function field(bytes) {
    const end = bytes.indexOf(0); const selected = end < 0 ? bytes : bytes.subarray(0, end);
    check((end < 0 || zero(bytes.subarray(end))) && selected.every(b => b >= 32 && b <= 126), 'Noncanonical archive string');
    return selected.toString('ascii');
  }
  function octal(bytes) {
    const raw = bytes.toString('latin1'); check(/^[ \0]*[0-7]+[ \0]*$/.test(raw), 'Noncanonical archive integer');
    const value = parseInt(raw.replace(/^[ \0]+|[ \0]+$/g, ''), 8); check(Number.isSafeInteger(value), 'Unsafe archive integer'); return value;
  }
  const files = new Map(); const seen = new Set(); let count = 0; let bytes = 0; let offset = 0; let ended = false;
  while (offset + 512 <= raw.length) {
    const header = raw.subarray(offset, offset + 512);
    if (zero(header)) {
      check(offset + 1024 <= raw.length && zero(raw.subarray(offset)), 'Archive end markers or trailing bytes refused'); ended = true; break;
    }
    check(++count <= maxMembers, 'Archive member bound refused');
    let checksum = 0;
    for (let i = 0; i < 512; ++i) checksum += i >= 148 && i < 156 ? 32 : header[i];
    check(checksum === octal(header.subarray(148, 156)), 'Archive header checksum mismatch');
    check(header.subarray(257, 265).equals(Buffer.from([117, 115, 116, 97, 114, 0, 48, 48])), 'Only the locked USTAR archive format is accepted');
    const prefix = field(header.subarray(345, 500)); const leaf = field(header.subarray(0, 100));
    const name = prefix ? prefix + '/' + leaf : leaf; const normalized = name.replace(/\/$/, '');
    const size = octal(header.subarray(124, 136)); const type = header[156];
    const rootDirectory = prefix === '' && (leaf === 'package' || leaf === 'package/') && type === 53 && size === 0;
    check((rootDirectory || /^package\/[A-Za-z0-9_@.+/-]+$/.test(normalized)) && path.posix.normalize(normalized) === normalized
      && normalized.split('/').every(p => p !== '' && p !== '.' && p !== '..') && !seen.has(normalized.toLowerCase()), 'Unsafe or duplicate archive member');
    seen.add(normalized.toLowerCase());
    check((type === 0 || type === 48 || type === 53) && zero(header.subarray(157, 257)) && size <= 8 << 20 && (type !== 53 || size === 0), 'Archive link, special member or size refused');
    bytes += size; check(bytes <= maxBytes, 'Archive expansion bound refused');
    const start = offset + 512; const end = start + size; const next = start + Math.ceil(size / 512) * 512;
    check(next <= raw.length && zero(raw.subarray(end, next)), 'Truncated or noncanonical archive data');
    if (type !== 53 && (name.endsWith('.sol') || name === 'package/package.json')) files.set(name, raw.subarray(start, end));
    offset = next;
  }
  check(ended, 'Archive end markers missing');
  return { files, members: count, declaredBytes: bytes };
}
function dependencySources(raw, pin, options = {}) {
  check(raw.length === pin.bytes && sha256(raw) === pin.sha256
    && 'sha512-' + crypto.createHash('sha512').update(raw).digest('base64') === pin.integrity, 'Locked dependency archive digest mismatch');
  let unpacked;
  const maximum = options.maxUnpackedBytes ?? 64 << 20;
  check(Number.isSafeInteger(maximum) && maximum > 0 && maximum <= 64 << 20, 'Invalid restricted decompression bound');
  try { unpacked = zlib.gunzipSync(raw, { maxOutputLength: maximum }); }
  catch { throw new BuildRefusal('Dependency archive decompression refused'); }
  const archive = tarSources(unpacked, { maxBytes: maximum }); const metadata = archive.files.get('package/package.json');
  check(metadata, 'Dependency package metadata missing');
  const packageInfo = parseJSON(metadata, 1 << 20);
  check(packageInfo.name === '@openzeppelin/contracts' && packageInfo.version === '5.6.1', 'Dependency package identity mismatch');
  const sources = new Map();
  for (const [name, raw] of archive.files) {
    if (name.endsWith('.sol')) sources.set('@openzeppelin/contracts/' + name.slice('package/'.length), raw);
  }
  return { sources, members: archive.members, declaredBytes: archive.declaredBytes };
}

const SOLC = Object.freeze({ version: '0.8.30+commit.73712a01.Emscripten.clang', bytes: 9591828, sha256: '81475c98b6d2094a821fd9d7b6278556d8095ccc23e0b8a1029b1c08a89cd4b2' });
function loadCompiler(filename, pin) {
  check(canonical(pin) === canonical(SOLC), 'Unsupported compiler lock');
  const verify = () => {
    const raw = readRegular(filename, 16 << 20);
    check(raw.length === pin.bytes && sha256(raw) === pin.sha256, 'Compiler digest or byte count mismatch');
  };
  verify(); const solc = require(path.resolve(filename));
  const version = solc.cwrap('solidity_version', 'string', [])(); check(version === pin.version, 'Compiler-reported version mismatch');
  const compile = solc.cwrap('solidity_compile', 'string', ['string', 'number', 'number']);
  const reset = solc.cwrap('solidity_reset', null, []);
  return {
    verify,
    compile(input) {
      const raw = JSON.stringify(input); check(Buffer.byteLength(raw) <= 16 << 20, 'Compiler input byte bound refused');
      let output;
      try { output = parseJSON(Buffer.from(compile(raw, 0, 0)), 8 << 20); }
      finally { reset(); }
      check(output.contracts && output.sources && !output.errors?.some(e => !['warning', 'info'].includes(e.severity)), 'Compiler JSON contains an error or incomplete output');
      return { schema: 1, compilerVersion: version, input, output };
    },
  };
}

module.exports = { BuildRefusal, check, sha256, canonical, text, readRegular, parseJSON, sourceUnit, imports, sourceClosure, tarSources, dependencySources, loadCompiler, SOLC };
