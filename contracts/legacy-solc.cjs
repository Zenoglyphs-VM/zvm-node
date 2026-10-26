'use strict';

const path = require('node:path');
const vm = require('node:vm');
const { check, canonical, sha256, text, readRegular, parseJSON } = require('./build-support.cjs');

const COMPILERS = Object.freeze({
  core: Object.freeze({ version: '0.5.16+commit.9c3226ce.Emscripten.clang', bytes: 7625538, sha256: 'a09c9cc2672678d461dc71100600bb58802db87be4de9424769241712ccbec03' }),
  periphery: Object.freeze({ version: '0.6.6+commit.6c089d02.Emscripten.clang', bytes: 8442418, sha256: '09f6098026622c5c334c7798c3ad2b8f7c0ebc62f87846c7d5e7e725c3d1cbc2' }),
});

function loadLegacyCompiler(filename, pin) {
  const group = Object.keys(COMPILERS).find(key => canonical(COMPILERS[key]) === canonical(pin));
  check(group, 'Unsupported legacy compiler lock');
  const read = () => {
    const raw = readRegular(filename, 16 << 20);
    check(raw.length === pin.bytes && sha256(raw) === pin.sha256, 'Legacy compiler digest or byte count mismatch');
    return raw;
  };
  const raw = read(); const absolute = path.resolve(filename); const loaded = { exports: {} };
  // Execute the verified captured bytes, without a second path read or a stale
  // CommonJS cache entry. Both exact upstream modules require only fs and path.
  const requireBuiltin = name => {
    check(name === 'fs' || name === 'path', 'Unexpected legacy compiler dependency');
    return require('node:' + name);
  };
  const execute = vm.compileFunction(text(raw), ['exports', 'require', 'module', '__filename', '__dirname'], { filename: 'verified-solc.js' });
  execute(loaded.exports, requireBuiltin, loaded, absolute, path.dirname(absolute));
  const solc = loaded.exports;
  check(typeof solc.cwrap === 'function' && typeof solc._solidity_compile === 'function'
    && typeof solc._solidity_version === 'function', 'Legacy compiler API missing');
  const version = solc.cwrap('solidity_version', 'string', [])();
  check(version === pin.version, 'Legacy compiler-reported version mismatch');
  const hasReset = typeof solc._solidity_reset === 'function';
  check(hasReset === (group === 'periphery'), 'Legacy compiler reset API differs from the reviewed module');
  const compile = solc.cwrap('solidity_compile', 'string', group === 'core' ? ['string', 'number'] : ['string', 'number', 'number']);
  const reset = hasReset ? solc.cwrap('solidity_reset', null, []) : null;
  let used = false;
  return {
    verify() { read(); },
    compile(input) {
      check(!used, 'Legacy compiler accepts one compilation per process'); used = true;
      const source = JSON.stringify(input);
      check(Buffer.byteLength(source) > 0 && Buffer.byteLength(source) <= 8 << 20, 'Legacy compiler input byte bound refused');
      let output;
      try { output = parseJSON(Buffer.from(compile(source, 0, 0)), 8 << 20); }
      finally { if (reset) reset(); }
      check(output.contracts && output.sources && (!output.errors || Array.isArray(output.errors))
        && !output.errors?.some(error => !['warning', 'info'].includes(error?.severity)), 'Legacy compiler JSON contains an error or incomplete output');
      return { schema: 1, compilerVersion: version, input, output };
    },
  };
}

module.exports = { COMPILERS, loadLegacyCompiler };
