import { readFileSync } from 'node:fs'

const [version] = process.argv.slice(2)
if (process.argv.length !== 3 || !/^v\d+\.\d+\.\d+$/.test(version ?? '')) {
  console.error('Usage: node scripts/release-notes.mjs <vX.Y.Z>')
  process.exit(1)
}

const lines = readFileSync('CHANGELOG.md', 'utf8').split(/\r?\n/)
const heading = new RegExp(`^## \\[${version.replaceAll('.', '\\.')}\\](?: - \\d{4}-\\d{2}-\\d{2})?\\s*$`)
const start = lines.findIndex(line => heading.test(line))
if (start === -1) {
  console.error(`CHANGELOG.md is missing a release section for ${version}; prepare it before tagging`)
  process.exit(1)
}
let end = start + 1
while (end < lines.length && !lines[end].startsWith('## ')) end++
const notes = lines.slice(start + 1, end).join('\n').trim()
if (!notes || !/^- \S/m.test(notes)) {
  console.error(`CHANGELOG.md release section for ${version} must contain change entries`)
  process.exit(1)
}
console.log(notes)
