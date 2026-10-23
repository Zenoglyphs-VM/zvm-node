'use strict';
const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const zlib = require('node:zlib');
const {test} = require('node:test');
const {BuildRefusal, canonical, sha256, parseJSON} = require('./build-support.cjs');
const {COMPILERS, loadLegacyCompiler} = require('./legacy-solc.cjs');
const {PACKAGES, SETTINGS, options, readPackage, material, compareCommitted} = require('./rebuild-uniswap.cjs');
const refuses = run => assert.throws(run, BuildRefusal);

function packageFixture(info) {
  const files = [['package/package.json', JSON.stringify(info)], ['package/contracts/A.sol', 'contract A {}']]; const blocks=[];
  for (const [name, text] of files) {
    const body=Buffer.from(text);const header=Buffer.alloc(512);header.write(name,0,100,'ascii');
    header.write('0000644\0',100,8,'ascii');header.write('0000000\0',108,8,'ascii');header.write('0000000\0',116,8,'ascii');
    header.write(body.length.toString(8).padStart(11,'0')+'\0',124,12,'ascii');header.write('00000000000\0',136,12,'ascii');
    header.fill(32,148,156);header[156]=48;Buffer.from([117,115,116,97,114,0,48,48]).copy(header,257);
    header.write(header.reduce((sum,byte)=>sum+byte,0).toString(8).padStart(6,'0')+'\0 ',148,8,'ascii');
    blocks.push(header,body,Buffer.alloc((512-body.length%512)%512));
  }
  blocks.push(Buffer.alloc(1024));const raw=zlib.gzipSync(Buffer.concat(blocks));
  return {raw,pin:{name:info.name,version:info.version,bytes:raw.length,sha256:sha256(raw),integrity:'sha512-'+crypto.createHash('sha512').update(raw).digest('base64')}};
}
function artifact() {
  return {abi:[],metadata:'{"compiler":{"version":"fixture"}}',storageLayout:{storage:[],types:null},evm:{
    bytecode:{object:'6000',linkReferences:{},opcodes:'PUSH1 0x0',sourceMap:'0:0:0'},
    deployedBytecode:{object:'00'.repeat(96),linkReferences:{},immutableReferences:{'7':[{start:16,length:32}]},opcodes:'STOP',sourceMap:'0:0:0'},
  }};
}
const clone = value => JSON.parse(JSON.stringify(value));
function committed(value) {return {abi:value.abi,interface:value.abi,metadata:value.metadata,bytecode:value.evm.bytecode.object,evm:clone(value.evm)};}

test('legacy modules and compiler settings remain fixed before any executable file is read', () => {
  assert.equal(COMPILERS.core.version,'0.5.16+commit.9c3226ce.Emscripten.clang');
  assert.equal(COMPILERS.periphery.version,'0.6.6+commit.6c089d02.Emscripten.clang');
  assert.throws(()=>{COMPILERS.core.sha256='changed';},TypeError);
  assert.throws(()=>{SETTINGS.optimizer.runs=1;},TypeError);
  refuses(()=>loadLegacyCompiler('nonexistent-compiler.fixture',{...COMPILERS.core,version:'0.8.30'}));
  refuses(()=>loadLegacyCompiler('nonexistent-compiler.fixture',{...COMPILERS.core,extra:true}));
});

test('core and nested periphery core versions have distinct exact flags and lock identities', () => {
  assert.equal(PACKAGES.find(p=>p.id==='core').version,'1.0.1');assert.equal(PACKAGES.find(p=>p.id==='periphery-core').version,'1.0.0');
  const base=['--group','core','--compiler','compiler','--core-archive','core'];assert.equal(options(base).group,'core');
  const router=['--group','periphery','--compiler','compiler','--periphery-archive','periphery','--periphery-core-archive','old-core','--lib-archive','lib'];
  assert.equal(options([...router,'--emit-bundle']).emit,true);
  for(const args of [[],['--group','unknown','--compiler','file'],[...base,'--group','core'],[...base,'--emit-bundle','--emit-bundle'],
    [...base,'--lib-archive','unused'],router.slice(0,-2),['--group','core','--compiler'],['--group','__proto__','--compiler','file'],
    ['--group','core','--compiler','--core-archive','file'],['--group','core','--compiler','file','--periphery-core-archive','wrong-version']])refuses(()=>options(args));
});

test('generic package reader binds both digests, exact version and cumulative expansion/member budgets', () => {
  const f=packageFixture({name:'@uniswap/v2-core',version:'1.0.0'});
  const got=readPackage(f.raw,f.pin);assert.equal(got.files.get('package/contracts/A.sol').toString(),'contract A {}');
  for(const pin of [{...f.pin,bytes:f.pin.bytes+1},{...f.pin,sha256:'0'.repeat(64)},{...f.pin,integrity:'sha512-wrong'},
    {...f.pin,version:'1.0.1'},{...f.pin,name:'@uniswap/v2-periphery'}])refuses(()=>readPackage(f.raw,pin));
  refuses(()=>readPackage(f.raw,f.pin,1024));refuses(()=>readPackage(f.raw,f.pin,32 << 20,1));
  refuses(()=>readPackage(f.raw,f.pin,0));refuses(()=>readPackage(f.raw,f.pin,32 << 20,0));
});

test('compiler material retains full metadata, opcode/source maps and exact immutable identities', () => {
  const a=artifact();const expected=material(a);assert.equal(expected.runtimeTemplateSha256,sha256(Buffer.from(a.evm.deployedBytecode.object,'hex')));
  for(const mutate of [x=>{x.metadata+=' ';},x=>{x.evm.deployedBytecode.opcodes+=' STOP';},x=>{x.evm.deployedBytecode.sourceMap+=';';},
    x=>{x.evm.deployedBytecode.immutableReferences={'8':[{start:16,length:32}]};},x=>{x.storageLayout.types={};}]){
    const changed=clone(a);mutate(changed);assert.notEqual(canonical(material(changed)),canonical(expected));
  }
});

test('immutable placeholders cannot be prefilled, overlapping, malformed or outside runtime', () => {
  for(const mutate of [x=>{x.evm.deployedBytecode.object='01'+x.evm.deployedBytecode.object.slice(2);x.evm.deployedBytecode.immutableReferences={'7':[{start:0,length:32}]};},
    x=>{x.evm.deployedBytecode.immutableReferences={'7':[{start:16,length:32}],'8':[{start:17,length:32}]};},
    x=>{x.evm.deployedBytecode.immutableReferences={'7':[{start:80,length:32}]};},x=>{x.evm.deployedBytecode.immutableReferences={'7':[{start:16,length:31}]};},
    x=>{x.evm.deployedBytecode.immutableReferences={'7':[{start:-1,length:32}]};},x=>{x.evm.deployedBytecode.immutableReferences={'01':[{start:16,length:32}]};},
    x=>{x.evm.deployedBytecode.immutableReferences=[];},x=>{x.evm.deployedBytecode.immutableReferences={'7':[]};},
    x=>{x.evm.bytecode.linkReferences={Library:{address:0}};},x=>{x.evm.bytecode.linkReferences=[];},x=>{x.evm.bytecode.object='__$unresolved$__';}]){
    const changed=artifact();mutate(changed);refuses(()=>material(changed));
  }
});

test('committed comparison refuses runtime masks, ABI aliases, reference movement and metadata changes', () => {
  const a=artifact();const original=committed(a);compareCommitted(a,original,true);
  for(const mutate of [x=>{x.evm.deployedBytecode.object+='00';},x=>{x.evm.bytecode.sourceMap+=';';},x=>{x.evm.deployedBytecode.immutableReferences['7'][0].start++;},
    x=>{x.interface=[{name:'changed'}];},x=>{x.bytecode+='00';},x=>{x.metadata+=' ';},x=>{delete x.metadata;}]){
    const changed=clone(original);mutate(changed);refuses(()=>compareCommitted(a,changed,true));
  }
  const router=clone(original);delete router.metadata;compareCommitted(a,router,false);
  refuses(()=>compareCommitted(a,original,false));
});

test('reviewed lock covers all thirty-nine compiler targets and exactly three committed subjects', () => {
  const fs=require('node:fs');const path=require('node:path');const lock=parseJSON(fs.readFileSync(path.join(__dirname,'uniswap-build.lock.json')));
  assert.equal(lock.groups.reduce((sum,g)=>sum+g.targets.length,0),39);
  assert.deepEqual(lock.groups.map(g=>[g.name,g.sources.length,g.targets.length,g.artifacts.length]),[['core',12,12,2],['periphery',27,27,1]]);
  assert.equal(canonical(lock.packages),canonical(PACKAGES));assert.equal(canonical(lock.compilers),canonical(COMPILERS));
});
