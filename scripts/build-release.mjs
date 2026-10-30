import { existsSync, mkdirSync, readFileSync } from 'node:fs'
import { spawnSync } from 'node:child_process'

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

mkdirSync('bin', { recursive: true })
for (const target of selectedTarget ? [selectedTarget] : targets) {
  const [goos, architecture] = target.split('/')
  const arm = /^armv([567])$/.exec(architecture)
  const goarch = arm ? 'arm' : architecture
  const output = `bin/sui-copilot-${goos}-${architecture}${goos === 'windows' ? '.exe' : ''}`
  console.log(`Building ${target} (${version})`)
  const result = spawnSync('go', [
    'build', '-trimpath', '-ldflags', `-s -w -X main.version=${version}`,
    '-o', output, './cmd/sui-copilot',
  ], {
    env: { ...process.env, GOOS: goos, GOARCH: goarch, GOARM: arm?.[1] ?? '', CGO_ENABLED: '0' },
    stdio: 'inherit',
  })
  if (result.error) console.error(result.error.message)
  if (result.status !== 0) process.exit(result.status ?? 1)
  console.log(`Built ${output}`)
}
