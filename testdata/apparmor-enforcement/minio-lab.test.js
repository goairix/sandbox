// 无网络/无 Kubernetes 写入的历史夹具安全回归。
const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const path = require('path');
const source = fs.readFileSync(path.join(__dirname, 'minio-lab.js'), 'utf8');

async function run() {
  for (const mode of ['setup', 'seed', 'seed-prefix', 'verify']) {
    let calls = 0;
    let writes = 0;
    const errors = [];
    const processStub = {argv: ['node', 'fixture', mode], exitCode: 0};
    const childStub = {
      spawn: () => {writes++; throw new Error('unexpected process');},
      spawnSync: (bin, args) => {
        calls++;
        assert.equal(bin, 'kubectl');
        assert.equal(args[4], 'get');
        return {status: 0, stdout: JSON.stringify({metadata: {uid: 'replacement', labels: {'sandbox-test-run': 'a18c24'}}})};
      },
    };
    vm.runInNewContext(source, {
      require: name => name === 'child_process' ? childStub : name === 'https' ? {
        request: () => {writes++; throw new Error('unexpected request');},
      } : require(name),
      process: processStub,
      Buffer,
      console: {error: message => errors.push(message), log: () => {}},
    });
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(processStub.exitCode, 1);
    assert.equal(calls, 1);
    assert.equal(writes, 0);
    assert.deepEqual(errors, ['lab namespace identity mismatch']);
  }
  console.log('all four fixture modes reject replacement UID before any write: PASS');
}
run().catch(error => {console.error(error); process.exitCode = 1;});
