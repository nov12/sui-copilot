import { createHash } from 'node:crypto'
import { chmodSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import { resolve } from 'node:path'

const targets = JSON.parse(readFileSync(new URL('./release-targets.json', import.meta.url), 'utf8'))
const [version, selectedTarget] = process.argv.slice(2)

if (!version || !/^[\w.+-]+$/.test(version) || process.argv.length > 4) {
  console.error('Usage: node scripts/build-release.mjs <version> [target]')
  process.exit(1)
}
if (selectedTarget && !targets.includes(selectedTarget)) {
  console.error(`Unsupported release target: ${selectedTarget}`)
  process.exit(1)
}
if (!existsSync('internal/httpapi/static/dist/index.html') || !existsSync('internal/httpapi/static/dist/assets')) {
  console.error('Frontend assets missing; run task web:embed before building release binaries')
  process.exit(1)
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, { stdio: 'inherit', ...options })
  if (result.error) console.error(result.error.message)
  if (result.status !== 0) process.exit(result.status ?? 1)
}

const buildTargets = selectedTarget ? [selectedTarget] : targets
for (const command of new Set(buildTargets.map(target => target.startsWith('windows/') ? 'zip' : 'tar'))) {
  run(command, ['--version'], { stdio: 'ignore' })
}

mkdirSync('bin', { recursive: true })
rmSync('bin/release', { recursive: true, force: true })
mkdirSync('bin/release', { recursive: true })
const checksums = []
for (const target of buildTargets) {
  const [goos, architecture] = target.split('/')
  const arm = /^armv([567])$/.exec(architecture)
  const goarch = arm ? 'arm' : architecture
  const base = `sui-copilot-${goos}-${architecture}`
  const binary = base + (goos === 'windows' ? '.exe' : '')
  const output = `bin/${binary}`
  console.log(`Building ${target} (${version})`)
  run('go', [
    'build', '-trimpath', '-ldflags', `-s -w -X sui-copilot/internal/buildinfo.Version=${version}`,
    '-o', output, './cmd/sui-copilot',
  ], {
    env: { ...process.env, GOOS: goos, GOARCH: goarch, GOARM: arm?.[1] ?? '', CGO_ENABLED: '0' },
  })
  chmodSync(output, 0o755)
  const archive = base + (goos === 'windows' ? '.zip' : '.tar.gz')
  const archivePath = `bin/release/${archive}`
  if (goos === 'windows') {
    run('zip', ['-q', '-j', archivePath, output])
  } else {
    run('tar', ['-czf', resolve(archivePath), '-C', resolve('bin'), binary])
  }
  const digest = createHash('sha256').update(readFileSync(archivePath)).digest('hex')
  checksums.push(`${digest}  ${archive}`)
  console.log(`Built ${archivePath}`)
}
writeFileSync('bin/release/SHA256SUMS', checksums.join('\n') + '\n')
