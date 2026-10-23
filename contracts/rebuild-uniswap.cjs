#!/usr/bin/env node
'use strict';

const path = require('node:path');
const crypto = require('node:crypto');
const zlib = require('node:zlib');
const {
  BuildRefusal, check, sha256, canonical, readRegular, parseJSON, sourceClosure, tarSources,
} = require('./build-support.cjs');
const { COMPILERS, loadLegacyCompiler } = require('./legacy-solc.cjs');

const PACKAGES = Object.freeze([
  Object.freeze({ id: 'core', name: '@uniswap/v2-core', version: '1.0.1', npmLockPath: 'node_modules/@uniswap/v2-core',
    url: 'https://registry.npmjs.org/@uniswap/v2-core/-/v2-core-1.0.1.tgz', bytes: 300338, sha256: 'f01288aaf73a89d2ff541b7276cec215ce2c480cafbed33b0da9c89853f976a2',
    integrity: 'sha512-MtybtkUPSyysqLY2U210NBDeCHX+ltHt3oADGdjqoThZaFRDKwM6k1Nb3F0A3hk5hwuQvytFWhrWHOEq6nVJ8Q==' }),
  Object.freeze({ id: 'periphery', name: '@uniswap/v2-periphery', version: '1.1.0-beta.0', npmLockPath: 'node_modules/@uniswap/v2-periphery',
    url: 'https://registry.npmjs.org/@uniswap/v2-periphery/-/v2-periphery-1.1.0-beta.0.tgz', bytes: 722620, sha256: 'c3f1cda4ee1e00e877eb15ab508e574dfd6ae5e742be97fef41105d84a198784',
    integrity: 'sha512-6dkwAMKza8nzqYiXEr2D86dgW3TTavUvCR0w2Tu33bAbM8Ah43LKAzH7oKKPRT5VJQaMi1jtkGs1E8JPor1n5g==' }),
  Object.freeze({ id: 'periphery-core', name: '@uniswap/v2-core', version: '1.0.0', npmLockPath: 'node_modules/@uniswap/v2-periphery/node_modules/@uniswap/v2-core',
    url: 'https://registry.npmjs.org/@uniswap/v2-core/-/v2-core-1.0.0.tgz', bytes: 274605, sha256: '612c3bf99c809dc41a478e930f1c42e3604509028bde424e8a0017bf74fea88d',
    integrity: 'sha512-BJiXrBGnN8mti7saW49MXwxDBRFiWemGetE58q8zgfnPPzQKq55ADltEILqOt6VFZ22kVeVKbF8gVd8aY3l7pA==' }),
  Object.freeze({ id: 'lib', name: '@uniswap/lib', version: '1.1.1', npmLockPath: 'node_modules/@uniswap/lib',
    url: 'https://registry.npmjs.org/@uniswap/lib/-/lib-1.1.1.tgz', bytes: 15900, sha256: 'fd480df65e078d483b0a40cc7f35fcd5bb0c1ccbcb40e741f1660e9e81a88328',
    integrity: 'sha512-2yK7sLpKIT91TiS5sewHtOa7YuM8IuBXVl4GZv2jZFys4D2sY7K5vZh6MqD25TPA95Od+0YzCVq6cTF2IKrOmg==' }),
]);
const DEPENDENCIES = Object.freeze({ core: ['core'], periphery: ['periphery', 'periphery-core', 'lib'] });
const SUBJECTS = Object.freeze({ core: ['UniswapV2Factory', 'UniswapV2Pair'], periphery: ['UniswapV2Router02'] });
function freeze(value) {
  if (value && typeof value === 'object') { Object.values(value).forEach(freeze); Object.freeze(value); } return value;
}
const SETTINGS = freeze({ optimizer: { enabled: true, runs: 999999 }, evmVersion: 'istanbul', libraries: {}, remappings: [],
  outputSelection: { '*': { '*': ['abi', 'metadata', 'storageLayout', 'evm.bytecode', 'evm.deployedBytecode'] } } });
const MATERIAL_FIELDS = ['abiSha256', 'creationSha256', 'runtimeTemplateSha256', 'evmSha256', 'storageLayoutSha256', 'metadataSha256', 'immutableReferences', 'linkReferences'];

function fields(value, names) {
  check(value && typeof value === 'object' && !Array.isArray(value)
    && canonical(Object.keys(value).sort()) === canonical([...names].sort()), 'Unsupported Uniswap build document fields');
}
function code(value) {
  check(typeof value === 'string' && value.length % 2 === 0 && /^[0-9a-f]*$/.test(value), 'Noncanonical or unresolved Uniswap bytecode');
  return Buffer.from(value, 'hex');
}
function readPackage(raw, pin, remainingBytes = 32 << 20, remainingMembers = 4096) {
  check(raw.length === pin.bytes && sha256(raw) === pin.sha256
    && 'sha512-' + crypto.createHash('sha512').update(raw).digest('base64') === pin.integrity, 'Locked Uniswap archive digest mismatch');
  const maximum = Math.min(32 << 20, remainingBytes);
  check(Number.isSafeInteger(maximum) && maximum > 0 && Number.isSafeInteger(remainingMembers)
    && remainingMembers > 0 && remainingMembers <= 4096, 'Uniswap cumulative archive bound refused');
  let unpacked;
  try { unpacked = zlib.gunzipSync(raw, { maxOutputLength: maximum }); }
  catch { throw new BuildRefusal('Uniswap archive decompression refused'); }
  const archive = tarSources(unpacked, { maxBytes: maximum, maxMembers: remainingMembers });
  const metadata = archive.files.get('package/package.json'); check(metadata, 'Uniswap package identity missing');
  const info = parseJSON(metadata, 1 << 20);
  check(info.name === pin.name && info.version === pin.version, 'Uniswap package identity mismatch');
  return { ...archive, unpackedBytes: unpacked.length };
}
function material(artifact) {
  const evm = artifact.evm;
  check(Array.isArray(artifact.abi) && evm?.bytecode && evm.deployedBytecode && artifact.storageLayout
    && typeof artifact.metadata === 'string', 'Uniswap compiler material missing');
  const creation = code(evm.bytecode.object); const runtime = code(evm.deployedBytecode.object);
  const links = { creation: evm.bytecode.linkReferences ?? {}, runtime: evm.deployedBytecode.linkReferences ?? {} };
  check([links.creation, links.runtime].every(value => value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === 0), 'Uniswap library links require a separate witness');
  const immutables = evm.deployedBytecode.immutableReferences ?? {};
  check(immutables && typeof immutables === 'object' && !Array.isArray(immutables), 'Invalid Uniswap immutable reference map');
  const ranges = [];
  for (const [id, entries] of Object.entries(immutables)) {
    check(/^(0|[1-9][0-9]*)$/.test(id) && Array.isArray(entries) && entries.length > 0 && entries.length <= 256, 'Invalid Uniswap immutable reference identity');
    for (const range of entries) {
      fields(range, ['start', 'length']);
      check(Number.isSafeInteger(range.start) && range.start >= 0 && range.length === 32 && range.start + 32 <= runtime.length,
        'Uniswap immutable reference outside its runtime');
      check(runtime.subarray(range.start, range.start + 32).every(byte => byte === 0), 'Uniswap runtime template has a prefilled immutable');
      ranges.push(range); check(ranges.length <= 256, 'Uniswap immutable reference bound refused');
    }
  }
  ranges.sort((a, b) => a.start - b.start);
  for (let i = 1; i < ranges.length; ++i) check(ranges[i].start >= ranges[i - 1].start + 32, 'Overlapping Uniswap immutable references');
  return { abiSha256: sha256(canonical(artifact.abi)), creationSha256: sha256(creation), runtimeTemplateSha256: sha256(runtime),
    evmSha256: sha256(canonical(evm)), storageLayoutSha256: sha256(canonical(artifact.storageLayout)), metadataSha256: sha256(artifact.metadata),
    immutableReferences: immutables, linkReferences: links };
}
function compareCommitted(artifact, original, expectsMetadata) {
  check(original && canonical(artifact.abi) === canonical(original.abi) && canonical(original.interface) === canonical(original.abi), 'Uniswap ABI or interface alias differs');
  check(original.bytecode === original.evm?.bytecode?.object && canonical(artifact.evm) === canonical(original.evm), 'Full Uniswap EVM artifact differs; metadata and reference maps are never masked');
  check(code(artifact.evm.bytecode.object).length > 0 && code(artifact.evm.deployedBytecode.object).length > 0, 'Selected Uniswap target has no executable code');
  check(Object.hasOwn(original, 'metadata') === expectsMetadata && (!expectsMetadata || original.metadata === artifact.metadata), 'Uniswap compiler metadata differs');
}
function options(argv) {
  const result = new Map(); let emit = false;
  for (let i = 0; i < argv.length; ++i) {
    const key = argv[i];
    if (key === '--emit-bundle' && !emit) { emit = true; continue; }
    check(['--group', '--compiler', ...PACKAGES.map(p => '--' + p.id + '-archive')].includes(key)
      && !result.has(key) && argv[i + 1] && !argv[i + 1].startsWith('--'), 'Use --group core|periphery --compiler FILE with the exact group archive flags');
    result.set(key, argv[++i]);
  }
  const group = result.get('--group');
  check(Object.hasOwn(DEPENDENCIES, group) && result.has('--compiler'), 'Uniswap group and compiler required');
  const required = ['--group', '--compiler', ...DEPENDENCIES[group].map(id => '--' + id + '-archive')];
  check(canonical([...result.keys()].sort()) === canonical(required.sort()), 'Missing or unrelated Uniswap archive flag');
  return { group, values: result, emit };
}

function rebuild(argv) {
  const cli = options(argv); const name = cli.group;
  const lockPath = path.join(__dirname, 'uniswap-build.lock.json'); const lockRaw = readRegular(lockPath, 1 << 20);
  const lock = parseJSON(lockRaw, 1 << 20);
  fields(lock, ['schema', 'compilers', 'packages', 'dependencyLockSha256', 'settings', 'groups']);
  check(lock.schema === 1 && canonical(lock.compilers) === canonical(COMPILERS)
    && canonical(lock.packages) === canonical(PACKAGES) && canonical(lock.settings) === canonical(SETTINGS), 'Unsupported Uniswap build lock');
  check(Array.isArray(lock.groups) && lock.groups.length === 2
    && canonical(lock.groups.map(g => g.name).sort()) === canonical(Object.keys(DEPENDENCIES).sort()), 'Incomplete or duplicate Uniswap build groups');
  const npmPath = path.join(__dirname, 'package-lock.json'); const npmRaw = readRegular(npmPath, 1 << 20);
  check(sha256(npmRaw) === lock.dependencyLockSha256, 'Uniswap dependency lock changed');
  const npm = parseJSON(npmRaw, 1 << 20);
  for (const pin of PACKAGES) {
    const entry = npm.packages?.[pin.npmLockPath];
    check(entry && entry.version === pin.version && entry.resolved === pin.url && entry.integrity === pin.integrity, 'Uniswap npm lock identity differs');
  }
  const group = lock.groups.find(g => g.name === name);
  fields(group, ['name', 'roots', 'inputSha256', 'sources', 'targets', 'artifacts']);
  check(Array.isArray(group.roots) && group.roots.length > 0 && group.roots.length <= 1024
    && new Set(group.roots).size === group.roots.length && Array.isArray(group.sources) && group.sources.length <= 1024
    && Array.isArray(group.targets) && group.targets.length > 0 && group.targets.length <= 1024, 'Uniswap build inventory bound refused');
  const archives = new Map(); const archivePins = []; let compressed = 0; let expanded = 0; let members = 0;
  for (const id of DEPENDENCIES[name]) {
    const pin = PACKAGES.find(p => p.id === id); const file = path.resolve(cli.values.get('--' + id + '-archive'));
    const raw = readRegular(file, 16 << 20); compressed += raw.length;
    check(compressed <= 16 << 20, 'Uniswap cumulative compressed archive bound refused');
    const archive = readPackage(raw, pin, (64 << 20) - expanded, 4096 - members);
    expanded += archive.unpackedBytes; members += archive.members;
    archives.set(id, archive); archivePins.push({ file, pin });
  }
  const units = new Map(); const primary = archives.get(name);
  for (const [file, raw] of primary.files) if (file.startsWith('package/contracts/') && file.endsWith('.sol')) units.set(file.slice(8), raw);
  check(canonical([...units.keys()].sort()) === canonical([...group.roots].sort()), 'Uniswap primary source roots differ');
  if (name === 'periphery') {
    for (const [id, prefix] of [['periphery-core', '@uniswap/v2-core/'], ['lib', '@uniswap/lib/']]) {
      for (const [file, raw] of archives.get(id).files) if (file.startsWith('package/contracts/') && file.endsWith('.sol')) units.set(prefix + file.slice(8), raw);
    }
  }
  const expected = new Map();
  for (const source of group.sources) {
    fields(source, ['unit', 'sha256']); check(!expected.has(source.unit), 'Duplicate Uniswap source unit'); expected.set(source.unit, source.sha256);
  }
  const sources = sourceClosure(group.roots, unit => {
    const raw = units.get(unit); check(expected.has(unit) && raw && sha256(raw) === expected.get(unit), 'Uniswap literal source digest differs'); return raw;
  });
  check(canonical(Object.keys(sources).sort()) === canonical([...expected.keys()].sort()), 'Missing or unused Uniswap source unit');
  const input = { language: 'Solidity', sources, settings: lock.settings };
  check(sha256(JSON.stringify(input)) === group.inputSha256, 'Uniswap compiler input/settings differs');
  check(Array.isArray(group.artifacts) && canonical(group.artifacts.map(a => a.name).sort()) === canonical(SUBJECTS[name]), 'Uniswap committed artifact inventory differs');
  const originals = new Map(); const originalPins = [];
  for (const item of group.artifacts) {
    fields(item, ['name', 'sha256']); const file = path.join(__dirname, '..', 'bench', 'artifacts', item.name + '.json');
    const raw = readRegular(file, 1 << 20); check(sha256(raw) === item.sha256, 'Committed Uniswap artifact bytes changed');
    originals.set('contracts/' + item.name + '.sol:' + item.name, parseJSON(raw, 1 << 20)); originalPins.push({ file, digest: item.sha256 });
  }
  const compiler = loadLegacyCompiler(path.resolve(cli.values.get('--compiler')), lock.compilers[name]); const bundle = compiler.compile(input);
  const compiled = new Map();
  for (const [unit, contracts] of Object.entries(bundle.output.contracts)) for (const [contract, artifact] of Object.entries(contracts)) compiled.set(unit + ':' + contract, artifact);
  const targetNames = group.targets.map(t => t.name);
  check(new Set(targetNames).size === targetNames.length && canonical([...compiled.keys()].sort()) === canonical([...targetNames].sort()), 'Uniswap compiler target inventory differs');
  const results = [];
  for (const target of group.targets) {
    fields(target, ['name', ...MATERIAL_FIELDS]); const artifact = compiled.get(target.name); const observed = material(artifact);
    for (const key of MATERIAL_FIELDS) check(canonical(observed[key]) === canonical(target[key]), 'Uniswap compiler material differs from the reviewed lock');
    if (originals.has(target.name)) compareCommitted(artifact, originals.get(target.name), name === 'core');
    results.push({ name: target.name, comparedToCommittedArtifact: originals.has(target.name), ...observed,
      creationBytes: code(artifact.evm.bytecode.object).length, runtimeTemplateBytes: code(artifact.evm.deployedBytecode.object).length });
  }
  compiler.verify(); check(sha256(readRegular(lockPath, 1 << 20)) === sha256(lockRaw)
    && sha256(readRegular(npmPath, 1 << 20)) === lock.dependencyLockSha256, 'Uniswap lock changed during compilation');
  for (const { file, pin } of archivePins) check(sha256(readRegular(file, 16 << 20)) === pin.sha256, 'Uniswap archive changed during compilation');
  for (const { file, digest } of originalPins) check(sha256(readRegular(file, 1 << 20)) === digest, 'Uniswap artifact changed during compilation');
  if (cli.emit) return bundle;
  return { schema: 1, group: name, compilerExecuted: true, compilerVersion: lock.compilers[name].version, compilerSha256: lock.compilers[name].sha256,
    inputSha256: group.inputSha256, sourceUnits: group.sources.length, compilerTargets: results.length, committedArtifactsCompared: originals.size,
    archiveCompressedBytes: compressed, archiveUnpackedBytes: expanded, archiveMembers: members, targets: results,
    metadataAllowances: [], libraryAllowances: [], immutableTreatment: 'Exact compiler maps and unpatched runtime templates; constructor substitution remains unqualified',
    constructorsExecuted: false, deploymentObserved: false, runtimeObserved: false, fullN03Coverage: false };
}

if (require.main === module) {
  try {
    const raw = JSON.stringify(rebuild(process.argv.slice(2)));
    check(Buffer.byteLength(raw) <= 8 << 20, 'Uniswap build output byte bound refused'); process.stdout.write(raw + '\n');
  } catch (error) {
    process.stderr.write((error instanceof BuildRefusal ? error.message : 'Uniswap rebuild refused; inspect inputs privately') + '\n'); process.exitCode = 1;
  }
}
module.exports = { PACKAGES, COMPILERS, SETTINGS, readPackage, material, compareCommitted, options, rebuild };
