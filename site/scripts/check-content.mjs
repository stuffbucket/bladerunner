// Content check for the landing site.
//
// The site describes how a first boot works. That behavior lives in Go code and
// in the README, so the copy goes stale in silence when the default changes.
// This check ties the two together:
//
//  1. It reads the default-image facts out of the repository (internal/config
//     and the README). If a fact moves, the check fails and tells you to revisit
//     the site copy along with it.
//  2. It rejects site copy that presents the cloud-init install path as the
//     normal first boot, unless the same passage labels it as the fallback.
//
// Run it with `npm run check:content`. `npm run build` runs it first (prebuild).
//
// What it cannot do: it matches strings, not meaning. Reworded stale copy that
// avoids every pattern below passes, and no check here can tell whether a timing
// claim on the page is still true. Treat it as a tripwire for the known drift,
// not as proof that the page is correct.

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const siteDir = join(dirname(fileURLToPath(import.meta.url)), '..');
const repoRoot = join(siteDir, '..');
const srcDir = join(siteDir, 'src');

/** Facts that must still hold in the repository for this site copy to be true. */
const REPO_FACTS = [
  {
    file: 'internal/config/config.go',
    pattern: /func DefaultBaseImageURL\(goarch string\) \(string, error\) \{\s*return HostedGuestImageURL\(goarch\)/,
    describes: 'the default base image resolves to the pre-baked hosted guest image',
  },
  {
    file: 'README.md',
    pattern: /default base image is the \*\*pre-baked bladerunner guest image\*\*/,
    describes: 'the README calls the pre-baked guest image the default',
  },
];

/**
 * Copy that only holds for the fallback path. A match is allowed when the
 * surrounding text labels it as the fallback (see FALLBACK_MARKER).
 */
const FALLBACK_ONLY = [
  { pattern: /cloud-init/gi, describes: 'cloud-init' },
  { pattern: /genericcloud/gi, describes: 'the genericcloud image' },
  {
    pattern: /first boot[^<]{0,120}?install(s|ing)? (and configures )?incus/gi,
    describes: 'first boot installing Incus',
  },
];

/** Words that mark a passage as the fallback path rather than the default. */
const FALLBACK_MARKER = /fallback|falls back|--debian-image|alternate|instead of the (pre-baked|default)/i;

/** How much text around a match is read when looking for FALLBACK_MARKER. */
const CONTEXT_CHARS = 320;

/** Copy the site must carry, so the default path stays described at all. */
const REQUIRED = [
  { pattern: /pre-baked/i, describes: 'the pre-baked default image' },
  { pattern: /falls back|fallback/i, describes: 'the fallback path' },
  { pattern: /genericcloud/i, describes: 'the genericcloud fallback image' },
];

/** Returns every .astro file under dir. */
function astroFiles(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) out.push(...astroFiles(path));
    else if (entry.endsWith('.astro')) out.push(path);
  }
  return out.sort();
}

/** Returns the 1-based line number of index in text. */
function lineOf(text, index) {
  return text.slice(0, index).split('\n').length;
}

const failures = [];

for (const fact of REPO_FACTS) {
  let source;
  try {
    source = readFileSync(join(repoRoot, fact.file), 'utf8');
  } catch {
    failures.push(`${fact.file}: not readable — run this check from a full repository checkout`);
    continue;
  }
  if (!fact.pattern.test(source)) {
    failures.push(
      `${fact.file}: no longer shows that ${fact.describes}. ` +
        'If the default changed, update the site copy and this check together.'
    );
  }
}

const files = astroFiles(srcDir);
const allCopy = [];

for (const path of files) {
  const text = readFileSync(path, 'utf8');
  allCopy.push(text);
  const name = relative(repoRoot, path);

  for (const rule of FALLBACK_ONLY) {
    rule.pattern.lastIndex = 0;
    let match;
    while ((match = rule.pattern.exec(text)) !== null) {
      const from = Math.max(0, match.index - CONTEXT_CHARS);
      const context = text.slice(from, match.index + match[0].length + CONTEXT_CHARS);
      if (FALLBACK_MARKER.test(context)) continue;
      failures.push(
        `${name}:${lineOf(text, match.index)}: mentions ${rule.describes} without labeling it ` +
          'the fallback. The default path boots the pre-baked image and does not install Incus.'
      );
    }
  }
}

const joined = allCopy.join('\n');
for (const rule of REQUIRED) {
  if (!rule.pattern.test(joined)) {
    failures.push(`site/src: no page describes ${rule.describes}`);
  }
}

if (failures.length > 0) {
  console.error('Site content check failed:');
  for (const failure of failures) console.error(`  - ${failure}`);
  process.exit(1);
}

console.log(`Site content check passed (${files.length} .astro files).`);
