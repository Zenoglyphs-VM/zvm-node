#!/usr/bin/env node
'use strict';

const path = require('node:path');
const crypto = require('node:crypto');
const { BuildRefusal, check, canonical, sha256, text, readRegular, parseJSON, sourceUnit, imports } = require('./build-support.cjs');
const { COMPILERS, loadUpstreamCompiler } = require('./upstream-solc.cjs');

const TARGETS = Object.freeze({
  multicall: [['Multicall3', 'Multicall3.sol', 'Multicall3', '0xca11bde05977b3631167028862be2a173976ca11']],
  op: [['L1Block', 'src/L2/L1Block.sol', 'L1Block', '0x4200000000000000000000000000000000000015'],
    ['L2ToL1MessagePasser', 'src/L2/L2ToL1MessagePasser.sol', 'L2ToL1MessagePasser', '0x4200000000000000000000000000000000000016']],
  permit2: [['Permit2', 'src/Permit2.sol', 'Permit2', '0x000000000022d473030f116ddee9f6b43ac78ba3']],
  entrypoint: [['EntryPoint_v07', 'contracts/core/EntryPoint.sol', 'EntryPoint', '0x0000000071727de22e5e9d8baf0edac6f37da032'],
    ['SenderCreator_v07', 'contracts/core/SenderCreator.sol', 'SenderCreator', '0xefc2c1444ebcc4db75e7613d20c6a62ff67a167c']],
  factory: [['Create2Deployer_Arachnid', 'deterministic-deployment-proxy.yul', 'Proxy', '0x4e59b44847b379578588920ca78fbf26c0b4956c'],
    ['SafeSingletonFactory', 'deterministic-deployment-proxy.yul', 'Proxy', '0x914d7fec6aac8cd542e72bca78b30650d45643d7']],
});
const HEX = /^[0-9a-f]{64}$/; const GIT = /^[0-9a-f]{40}$/;
function fields(value, names) {
  check(value && typeof value === 'object' && !Array.isArray(value)
    && canonical(Object.keys(value).sort()) === canonical([...names].sort()), 'Unsupported upstream build fields');
}
function code(value, empty = false) {
  check(typeof value === 'string' && (empty || value.length > 0) && value.length % 2 === 0
    && /^[0-9a-f]*$/.test(value) && value.length <= 2 << 20, 'Unresolved or noncanonical upstream bytecode');
  return Buffer.from(value, 'hex');
}
function gitBlob(raw) { return crypto.createHash('sha1').update(Buffer.from(`blob ${raw.length}\0`)).update(raw).digest('hex'); }
function origin(value) {
  check(value && typeof value === 'object', 'Missing source origin');
  if (value.kind === 'git') {
    fields(value, ['kind', 'repository', 'commit', 'path', 'blob']);
    check(/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(value.repository) && GIT.test(value.commit) && GIT.test(value.blob)
      && typeof value.path === 'string' && !value.path.startsWith('/') && path.posix.normalize(value.path) === value.path
      && value.path.split('/').every(p => p && p !== '.' && p !== '..' && p !== '.git'), 'Invalid Git source origin');
  } else {
    fields(value, ['kind', 'package', 'version', 'member', 'archiveSha256', 'archiveBytes', 'integrity']);
    check(value.kind === 'npm' && value.package === '@openzeppelin/contracts' && value.version === '5.0.0'
      && /^package\/(?:[A-Za-z0-9_-]+\/)*[A-Za-z0-9_-]+\.sol$/.test(value.member)
      && value.archiveBytes === 197500 && value.archiveSha256 === 'ad26e76e016825d34887c339cf4f47fccb84ba79e42ead14c7eb5bee1fda159e'
      && value.integrity === 'sha512-bv2sdS6LKqVVMLI5+zqnNrNU/CA+6z6CmwFXm/MzmOPBRSO5reEJN7z0Gbzvs0/bv/MZZXNklubpwy3v2+azsw==', 'Invalid npm source origin');
  }
}
function validateLock(lock) {
  fields(lock, ['schema', 'kind', 'compilerRepository', 'compilerCommit', 'dependencySourceLock', 'groups']);
  check(lock.schema === 1 && lock.kind === 'upstream-predeploy-source-reconstruction'
    && lock.compilerRepository === 'argotorg/solc-bin' && lock.compilerCommit === 'c04c6cdc827bbdcc7bd65689fa76b63a238b223f', 'Unsupported upstream reconstruction lock');
  fields(lock.dependencySourceLock, ['repository', 'commit', 'path', 'blob', 'sha256']);
  check(lock.dependencySourceLock.repository === 'eth-infinitism/account-abstraction' && GIT.test(lock.dependencySourceLock.commit)
    && lock.dependencySourceLock.path === 'yarn.lock' && GIT.test(lock.dependencySourceLock.blob) && HEX.test(lock.dependencySourceLock.sha256), 'Invalid dependency source lock');
  check(Array.isArray(lock.groups) && canonical(lock.groups.map(g => g.name)) === canonical(Object.keys(TARGETS)), 'Incomplete, reordered or duplicate upstream group inventory');
  for (const group of lock.groups) {
    fields(group, ['name', 'compiler', 'compilerSource', 'language', 'settings', 'inputSha256', 'outputContractsSha256', 'sources', 'targets']);
    check(canonical(group.compiler) === canonical(COMPILERS[group.name]) && [group.inputSha256, group.outputContractsSha256].every(h => HEX.test(h)), 'Unsupported compiler or input/output pin');
    fields(group.compilerSource, ['path', 'blob']);
    const directory = group.name === 'factory' ? 'wasm/' : 'bin/';
    check(group.compilerSource.path === directory + 'soljson-v' + group.compiler.version.replace('.Emscripten.clang', '') + '.js'
      && GIT.test(group.compilerSource.blob) && group.language === (group.name === 'factory' ? 'Yul' : 'Solidity'), 'Unexpected compiler source or language');
    check(group.settings && typeof group.settings === 'object' && Array.isArray(group.sources) && group.sources.length > 0 && group.sources.length <= 128, 'Invalid source/settings inventory');
    const units = new Set();
    for (const source of group.sources) {
      fields(source, ['unit', 'bytes', 'sha256', 'origin', 'equivalentOrigins']);
      check((sourceUnit(source.unit) || (group.name === 'factory' && source.unit === 'deterministic-deployment-proxy.yul'))
        && !units.has(source.unit) && Number.isSafeInteger(source.bytes) && source.bytes > 0 && source.bytes <= 1 << 20 && HEX.test(source.sha256), 'Invalid or duplicate locked source');
      units.add(source.unit); origin(source.origin);
      check(Array.isArray(source.equivalentOrigins) && source.equivalentOrigins.length <= 1, 'Invalid equivalent source origins');
      for (const equivalent of source.equivalentOrigins) { origin(equivalent); check(equivalent.kind === 'git', 'Equivalent source must be Git data'); }
    }
    check(Array.isArray(group.targets) && group.targets.length === TARGETS[group.name].length, 'Incomplete upstream target inventory');
    for (let i = 0; i < group.targets.length; i++) {
      const target = group.targets[i];
      fields(target, ['name', 'unit', 'contract', 'address', 'coreRuntime', 'coreFileSha256', 'creationBytes', 'creationSha256', 'runtimeTemplateBytes', 'runtimeTemplateSha256', 'runtimeSha256', 'abiSha256', 'metadataSha256', 'storageLayoutSha256', 'immutableReferences', 'immutableWords', 'historicalContext']);
      check(canonical([target.name, target.unit, target.contract, target.address]) === canonical(TARGETS[group.name][i])
        && units.has(target.unit) && target.coreRuntime === `l2/predeploys/${target.name}.hex`
        && [target.coreFileSha256, target.creationSha256, target.runtimeTemplateSha256, target.runtimeSha256, target.abiSha256, target.metadataSha256, target.storageLayoutSha256].every(h => HEX.test(h))
        && Number.isSafeInteger(target.creationBytes) && target.creationBytes > 0 && target.creationBytes <= 65536
        && Number.isSafeInteger(target.runtimeTemplateBytes) && target.runtimeTemplateBytes > 0 && target.runtimeTemplateBytes <= 65536, 'Invalid upstream target pins');
      check(target.immutableReferences && typeof target.immutableReferences === 'object' && !Array.isArray(target.immutableReferences)
        && target.immutableWords && typeof target.immutableWords === 'object' && !Array.isArray(target.immutableWords), 'Invalid immutable declarations');
      check(canonical(Object.keys(target.immutableReferences).sort()) === canonical(Object.keys(target.immutableWords).sort()), 'Immutable word inventory differs');
      for (const word of Object.values(target.immutableWords)) check(HEX.test(word), 'Invalid immutable word');
      if (target.name === 'Permit2') {
        fields(target.historicalContext, ['chainId', 'address', 'domainType', 'name']);
        check(target.historicalContext.chainId === 10 && target.historicalContext.address === target.address
          && target.historicalContext.domainType === 'EIP712Domain(string name,uint256 chainId,address verifyingContract)'
          && target.historicalContext.name === 'Permit2', 'Unexpected Permit2 historical context');
      } else if (target.name === 'EntryPoint_v07') {
        fields(target.historicalContext, ['parentAddress', 'childNonce', 'childAddress']);
        check(target.historicalContext.parentAddress === target.address && target.historicalContext.childNonce === 1
          && target.historicalContext.childAddress === TARGETS.entrypoint[1][3], 'Unexpected EntryPoint historical context');
      } else check(target.historicalContext === null && Object.keys(target.immutableReferences).length === 0, 'Unexpected immutable context');
    }
  }
  return lock;
}
function sourcesFor(rawBundle, group) {
  fields(rawBundle, ['schema', 'group', 'sources']);
  check(rawBundle.schema === 1 && rawBundle.group === group.name && rawBundle.sources && typeof rawBundle.sources === 'object'
    && !Array.isArray(rawBundle.sources), 'Unsupported literal source bundle');
  const expected = group.sources.map(s => s.unit).sort();
  check(canonical(Object.keys(rawBundle.sources).sort()) === canonical(expected), 'Missing or extra literal source units');
  let bytes = 0; const sources = {};
  for (const source of [...group.sources].sort((a, b) => a.unit < b.unit ? -1 : a.unit > b.unit ? 1 : 0)) {
    const entry = rawBundle.sources[source.unit]; fields(entry, ['content']);
    check(typeof entry.content === 'string', 'Only literal source content is accepted');
    const raw = Buffer.from(entry.content); bytes += raw.length;
    check(bytes <= 4 << 20 && raw.length === source.bytes && sha256(raw) === source.sha256, 'Literal source digest or byte count mismatch');
    for (const from of [source.origin, ...source.equivalentOrigins]) if (from.kind === 'git') check(gitBlob(raw) === from.blob, 'Literal source Git blob mismatch');
    sources[source.unit] = { content: entry.content };
  }
  if (group.language === 'Solidity') {
    const visited = new Set(); const pending = group.targets.map(t => t.unit);
    while (pending.length) {
      const unit = pending.pop(); if (visited.has(unit)) continue;
      check(Object.hasOwn(sources, unit), 'Literal import absent from the complete source inventory'); visited.add(unit);
      for (const item of imports(sources[unit].content)) {
        let next = item.startsWith('.') ? path.posix.normalize(path.posix.join(path.posix.dirname(unit), item)) : item;
        if (group.name === 'permit2' && next.startsWith('solmate/')) next = 'lib/solmate/' + next.slice(8);
        check(sourceUnit(next), 'Import escapes its declared source unit'); pending.push(next);
      }
    }
    check(canonical([...visited].sort()) === canonical(expected), 'Unused or incomplete literal import closure');
  }
  return sources;
}
function compareRuntime(template, expected, references, words) {
  check(template.length === expected.length && references && words, 'Runtime or immutable inventory length differs');
  check(canonical(Object.keys(references).sort()) === canonical(Object.keys(words).sort()), 'Immutable word inventory differs');
  const covered = new Set(); let ranges = 0;
  for (const [id, locations] of Object.entries(references)) {
    check(HEX.test(words[id]) && Array.isArray(locations) && locations.length > 0 && locations.length <= 128, 'Invalid immutable binding');
    for (const location of locations) {
      fields(location, ['start', 'length']); const { start, length } = location;
      check(Number.isSafeInteger(start) && start >= 0 && length === 32 && start + length <= template.length, 'Invalid immutable byte range');
      for (let i = start; i < start + length; i++) { check(!covered.has(i) && template[i] === 0, 'Overlapping or nonzero immutable template'); covered.add(i); }
      check(expected.subarray(start, start + length).toString('hex') === words[id], 'Committed immutable differs from its declared historical binding'); ranges++;
    }
  }
  for (let i = 0; i < template.length; i++) if (!covered.has(i)) check(template[i] === expected[i], 'Runtime differs outside exact compiler immutable ranges');
  return { fullRuntimeEqual: template.equals(expected), immutableRanges: ranges, metadataStripped: false };
}
function materialFor(artifact, target, factory) {
  check(artifact && artifact.evm?.bytecode && (factory || artifact.evm.deployedBytecode), 'Upstream compiler artifact missing');
  const evm = artifact.evm; const deployed = evm.deployedBytecode ?? {}; const creation = code(evm.bytecode.object);
  check(Object.keys(evm.bytecode.linkReferences ?? {}).length === 0 && Object.keys(deployed.linkReferences ?? {}).length === 0, 'Library links require a separate witness');
  let runtime = code(deployed.object ?? '', true);
  if (factory) {
    const prefix = Buffer.from('604580600e600039806000f350fe', 'hex');
    check(runtime.length === 0 && creation.length === 83 && creation.subarray(0, 14).equals(prefix), 'Unexpected Yul constructor copy prefix');
    runtime = creation.subarray(14);
    check(artifact.metadata === undefined && artifact.storageLayout === undefined && artifact.abi === undefined, 'Unexpected Yul artifact schema');
  } else check(typeof artifact.metadata === 'string' && artifact.metadata.length > 0 && Array.isArray(artifact.abi) && artifact.storageLayout, 'Solidity metadata, ABI or layout missing');
  const observed = { creationBytes: creation.length, creationSha256: sha256(creation), runtimeTemplateBytes: runtime.length,
    runtimeTemplateSha256: sha256(runtime), abiSha256: sha256(canonical(artifact.abi ?? null)),
    metadataSha256: sha256(artifact.metadata ?? ''), storageLayoutSha256: sha256(canonical(artifact.storageLayout ?? null)),
    immutableReferences: deployed.immutableReferences ?? {} };
  for (const [key, value] of Object.entries(observed)) check(canonical(value) === canonical(target[key]), 'Rebuilt upstream material differs from the reviewed lock');
  return { creation, runtime, observed };
}
function reconstruct(argv) {
  const options = new Map(); let emit = false;
  for (let i = 0; i < argv.length; i++) {
    const key = argv[i];
    if (key === '--emit-bundle' && !emit) { emit = true; continue; }
    check(['--group', '--compiler', '--source-bundle', '--core'].includes(key) && !options.has(key)
      && argv[i + 1] && !argv[i + 1].startsWith('--'), 'Use --group NAME --compiler FILE --source-bundle FILE --core DIRECTORY [--emit-bundle]');
    options.set(key, argv[++i]);
  }
  check(options.size === 4 && Object.hasOwn(TARGETS, options.get('--group')), 'One explicit upstream build group and all inputs are required');
  const lockFile = path.join(__dirname, 'upstream-predeploy-build.lock.json'); const lockRaw = readRegular(lockFile, 256 << 10);
  const lock = validateLock(parseJSON(lockRaw, 256 << 10)); const group = lock.groups.find(g => g.name === options.get('--group'));
  const sourceFile = path.resolve(options.get('--source-bundle')); const rawSources = readRegular(sourceFile, 8 << 20);
  const input = { language: group.language, sources: sourcesFor(parseJSON(rawSources), group), settings: group.settings };
  check(sha256(canonical(input)) === group.inputSha256, 'Complete compiler input/settings digest mismatch');
  const expectedFiles = group.targets.map(target => {
    const filename = path.join(path.resolve(options.get('--core')), target.coreRuntime); const raw = readRegular(filename, 256 << 10);
    check(sha256(raw) === target.coreFileSha256, 'Committed upstream runtime file differs from lock');
    const runtime = code(text(raw).trim().replace(/^0x/, ''));
    check(runtime.length === target.runtimeTemplateBytes && sha256(runtime) === target.runtimeSha256, 'Complete committed runtime differs from lock');
    return { filename, raw, runtime };
  });
  const compiler = loadUpstreamCompiler(path.resolve(options.get('--compiler')), group.name, group.compiler);
  const bundle = compiler.compile(input);
  check(sha256(canonical(bundle.output.contracts)) === group.outputContractsSha256, 'Complete compiler contract inventory/material differs from lock');
  const contracts = group.targets.map((target, index) => {
    const artifact = bundle.output.contracts[target.unit]?.[target.contract];
    const material = materialFor(artifact, target, group.name === 'factory');
    const comparison = compareRuntime(material.runtime, expectedFiles[index].runtime, target.immutableReferences, target.immutableWords);
    return { name: target.name, address: target.address, ...material.observed, runtimeSha256: target.runtimeSha256,
      ...comparison, immutableWords: target.immutableWords, declaredHistoricalContext: target.historicalContext,
      historicalContextDerivedThisRun: false, constructorExecuted: false };
  });
  compiler.verify();
  check(readRegular(sourceFile, 8 << 20).equals(rawSources) && readRegular(lockFile, 256 << 10).equals(lockRaw), 'Source bundle or build lock changed during compilation');
  for (const file of expectedFiles) check(readRegular(file.filename, 256 << 10).equals(file.raw), 'Committed runtime changed during compilation');
  return { schema: 1, kind: 'upstream-predeploy-source-reconstruction', group: group.name, compilerExecuted: true,
    compilerVersion: group.compiler.version, compilerSha256: group.compiler.sha256, inputSha256: group.inputSha256,
    sourceUnits: group.sources.length, outputContractsSha256: group.outputContractsSha256, contracts,
    constructorExecuted: false, currentGenesisCallsExecuted: false, genesisConstructed: false, releaseAccepted: false,
    fullTwelvePredeployCoverage: false, ...(emit ? { compilerBundle: bundle } : {}) };
}
if (require.main === module) {
  try {
    const raw = JSON.stringify(reconstruct(process.argv.slice(2)));
    check(Buffer.byteLength(raw) <= 8 << 20, 'Upstream reconstruction output bound refused'); process.stdout.write(raw + '\n');
  } catch (error) {
    process.stderr.write((error instanceof BuildRefusal ? error.message : 'Upstream predeploy reconstruction refused; inspect inputs privately') + '\n'); process.exitCode = 1;
  }
}
module.exports = { TARGETS, validateLock, sourcesFor, compareRuntime, materialFor, reconstruct };
