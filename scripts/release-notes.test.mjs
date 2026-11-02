import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const script = fileURLToPath(new URL('./release-notes.mjs', import.meta.url))

function generate(t, changelog, ...args) {
  const cwd = mkdtempSync(join(tmpdir(), 'sui-release-notes-'))
  t.after(() => rmSync(cwd, { recursive: true, force: true }))
  writeFileSync(join(cwd, 'CHANGELOG.md'), changelog)
  return spawnSync(process.execPath, [script, ...args], { cwd, encoding: 'utf8' })
}

test('publishes only the requested version, preserving its Markdown', t => {
  const notes = '本版改善命令行使用体验。\n\n### 修复\n\n- **帮助**：支持 `update -h`。\n\n```sh\nsui-copilot update -h\n```'
  const result = generate(t, `# 更新日志\n\n## [Unreleased]\n\n- 尚未发布\n\n## [v1.2.3] - 2026-11-02\n\n${notes}\n\n## [v1.2.2]\n\n- 旧版本\n`, 'v1.2.3')
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout, notes + '\n')
})

test('accepts CRLF and a final version section without a trailing newline', t => {
  const result = generate(t, '# 更新日志\r\n\r\n## [v1.2.3]\r\n\r\n### 修复\r\n\r\n- 修复解析失败', 'v1.2.3')
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout, '### 修复\n\n- 修复解析失败\n')
})

test('requires an exact version and never falls back to Unreleased or another version', t => {
  const result = generate(t, '## [Unreleased]\n\n- 新变化\n\n## [v1.2.30]\n\n- 其他版本\n', 'v1.2.3')
  assert.notEqual(result.status, 0)
  assert.equal(result.stdout, '')
  assert.match(result.stderr, /missing a release section for v1\.2\.3/)
})

for (const content of ['', '### 修复\n\n### 改进']) {
  test(`rejects a release without change entries: ${JSON.stringify(content)}`, t => {
    const result = generate(t, `## [v1.2.3]\n\n${content}\n\n## [v1.2.2]\n\n- 旧版本`, 'v1.2.3')
    assert.notEqual(result.status, 0)
    assert.equal(result.stdout, '')
    assert.match(result.stderr, /must contain change entries/)
  })
}

for (const args of [[], ['Unreleased'], ['v1x2x3'], ['v1.2.3', 'extra']]) {
  test(`rejects invalid release arguments: ${JSON.stringify(args)}`, t => {
    const result = generate(t, '## [v1.2.3]\n\n- 修复', ...args)
    assert.notEqual(result.status, 0)
    assert.equal(result.stdout, '')
    assert.match(result.stderr, /Usage:/)
  })
}
