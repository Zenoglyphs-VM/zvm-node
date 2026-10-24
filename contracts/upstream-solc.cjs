'use strict';

const path = require('node:path');
const vm = require('node:vm');
const { check, canonical, sha256, text, readRegular, parseJSON } = require('./build-support.cjs');

// Only these captured upstream modules have been reviewed with this ABI.
const COMPILERS = Object.freeze({
  multicall: Object.freeze({ version: '0.8.12+commit.f00d7308.Emscripten.clang', bytes: 8396944, sha256: 'c2c4738c96ad329cbb9baea615ed50ffb5a53d93fed8e00785e47242581d3c60', arguments: 3, reset: true }),
  op: Object.freeze({ version: '0.8.15+commit.e14f2714.Emscripten.clang', bytes: 8479007, sha256: '71135e459d691767ce3453bab4564ef4a640dd50182da36517cbc1f96c1d4c7c', arguments: 3, reset: true }),
  permit2: Object.freeze({ version: '0.8.17+commit.8df45f5f.Emscripten.clang', bytes: 8468401, sha256: '617828e63be485c7cc2dbcbdd5a22b582b40fafaa41016ad595637b83c90656c', arguments: 3, reset: true }),
  entrypoint: Object.freeze({ version: '0.8.23+commit.f704f362.Emscripten.clang', bytes: 8656105, sha256: '9c681b165c8647867589c0a5ecdc8692637a935928a2b1bbea2ff4a1f4976985', arguments: 3, reset: true }),
  factory: Object.freeze({ version: '0.5.8+commit.23d335f2.Emscripten.clang', bytes: 7478654, sha256: '25cfdd733e9c780ab85373268fde7bfa2e4b22093af57422ca3b586c7af7cd60', arguments: 2, reset: false }),
});

function loadUpstreamCompiler(filename, group, pin) {
  check(Object.hasOwn(COMPILERS, group) && canonical(pin) === canonical(COMPILERS[group]), 'Unsupported upstream compiler lock');
  const read = () => {
    const raw = readRegular(filename, 16 << 20);
    check(raw.length === pin.bytes && sha256(raw) === pin.sha256, 'Upstream compiler digest or byte count mismatch');
    return raw;
  };
  const raw = read(); const loaded = { exports: {} }; const absolute = path.resolve(filename);
  const requireBuiltin = name => {
    check(name === 'fs' || name === 'path', 'Unexpected upstream compiler dependency');
    return require('node:' + name);
  };
  const execute = vm.compileFunction(text(raw), ['exports', 'require', 'module', '__filename', '__dirname'], { filename: 'verified-upstream-solc.js' });
  execute(loaded.exports, requireBuiltin, loaded, absolute, path.dirname(absolute));
  const solc = loaded.exports;
  check(typeof solc.cwrap === 'function' && typeof solc._solidity_version === 'function'
    && typeof solc._solidity_compile === 'function', 'Upstream compiler API missing');
  const version = solc.cwrap('solidity_version', 'string', [])();
  check(version === pin.version && (typeof solc._solidity_reset === 'function') === pin.reset, 'Upstream compiler version or reset API mismatch');
  const compile = solc.cwrap('solidity_compile', 'string', pin.arguments === 2 ? ['string', 'number'] : ['string', 'number', 'number']);
  const reset = pin.reset ? solc.cwrap('solidity_reset', null, []) : null;
  let used = false;
  return {
    verify() { read(); },
    compile(input) {
      check(!used, 'One upstream compilation per process is required'); used = true;
      const source = JSON.stringify(input);
      check(Buffer.byteLength(source) > 0 && Buffer.byteLength(source) <= 8 << 20, 'Upstream compiler input byte bound refused');
      let output;
      try { output = parseJSON(Buffer.from(compile(source, 0, 0)), 8 << 20); }
      finally { if (reset) reset(); }
      check(output.contracts && typeof output.contracts === 'object' && !Array.isArray(output.contracts)
        && (!output.errors || Array.isArray(output.errors))
        && !output.errors?.some(error => !['warning', 'info'].includes(error?.severity)), 'Upstream compiler returned incomplete output or an error');
      return { schema: 1, compilerVersion: version, input, output };
    },
  };
}

module.exports = { COMPILERS, loadUpstreamCompiler };
