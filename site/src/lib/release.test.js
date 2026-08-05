// Unit test for the landing page's release selection (issue #235).
//
// Run with `npm test` in site/. It uses node:test and node:assert, so it needs
// no test framework and adds no dependency to site/package.json.
//
// The fixture below is the real shape of
// https://api.github.com/repos/stuffbucket/bladerunner/releases on the day the
// bug was reported: three guest-image releases newer than the newest product
// release, which is exactly the state GitHub's /releases/latest marker resolved
// to a qcow2-only release.

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  hasCliAsset,
  isAdvertisableRelease,
  isProductRelease,
  isProductTag,
  latestProductTag,
  resolveAdvertisedTag,
  versionFromTag,
} from './release.js';

/** @param {string[]} names */
const assets = (names) => names.map((name) => ({ name }));

const guestImage = (tag) => ({
  tag_name: tag,
  draft: false,
  prerelease: false,
  assets: assets([
    'bladerunner-guest-amd64.qcow2',
    'bladerunner-guest-amd64.qcow2.sha256',
    'bladerunner-guest-arm64.qcow2',
    'bladerunner-guest-arm64.qcow2.sha256',
  ]),
});

const product = (tag, version) => ({
  tag_name: tag,
  draft: false,
  prerelease: false,
  assets: assets([
    `bladerunner_${version}_darwin_aarch64.dmg`,
    `bladerunner_${version}_darwin_aarch64.dmg.sha256`,
    `bladerunner_${version}_darwin_aarch64.tar.gz`,
    'checksums.txt',
  ]),
});

// Newest-first, as GitHub returns it: the guest images sit above the CLI.
const LIVE_RELEASES = [
  guestImage('guest-image-v2026.08.05'),
  guestImage('guest-image-v2026.07.30'),
  guestImage('guest-image-v2026.07.20'),
  product('v0.4.7', '0.4.7'),
  guestImage('guest-image-v2026.06.22'),
  guestImage('guest-image-latest'),
  product('v0.4.6', '0.4.6'),
];

test('a newer guest image does not become the advertised version', () => {
  assert.equal(resolveAdvertisedTag('', LIVE_RELEASES), 'v0.4.7');
  assert.equal(versionFromTag(resolveAdvertisedTag('', LIVE_RELEASES)), '0.4.7');
});

test('guest-image tags are not product tags', () => {
  assert.equal(isProductTag('guest-image-v2026.08.05'), false);
  assert.equal(isProductTag('guest-image-latest'), false);
  assert.equal(isProductTag('v0.4.7'), true);
  assert.equal(isProductTag('v0.4.7-rc.1'), false);
  assert.equal(isProductTag('v1.2'), false);
});

test('a guest-image tag would render verbatim if it were ever selected', () => {
  // The `^v` anchor never matched `guest-image-v...`, so mis-selection showed
  // the whole tag on the page. Selection is the fix; this records the reason.
  assert.equal(versionFromTag('guest-image-v2026.08.05'), 'guest-image-v2026.08.05');
});

test('a release must carry an installable CLI asset', () => {
  assert.equal(hasCliAsset(product('v0.4.7', '0.4.7')), true);
  assert.equal(hasCliAsset(guestImage('guest-image-v2026.08.05')), false);
  assert.equal(hasCliAsset({ assets: assets(['checksums.txt']) }), false);
  assert.equal(hasCliAsset({ assets: assets(['Bladerunner.dmg']) }), true);
  assert.equal(hasCliAsset({ assets: assets(['Bladerunner.app.tar.gz']) }), true);
  assert.equal(hasCliAsset({}), false);

  // A well-formed product tag with no CLI artifact is not advertisable.
  const empty = { tag_name: 'v9.9.9', draft: false, prerelease: false, assets: [] };
  assert.equal(isProductRelease(empty), false);
  assert.equal(latestProductTag([empty, ...LIVE_RELEASES]), 'v0.4.7');
});

test('drafts and prereleases are excluded', () => {
  const draft = { ...product('v0.5.0', '0.5.0'), draft: true };
  const pre = { ...product('v0.5.1', '0.5.1'), prerelease: true };
  assert.equal(latestProductTag([draft, pre, ...LIVE_RELEASES]), 'v0.4.7');
  assert.equal(isAdvertisableRelease(draft), false);
  // A draft cannot be pinned to either — its links would 404.
  assert.equal(resolveAdvertisedTag('v0.5.0', [draft, ...LIVE_RELEASES]), 'v0.4.7');
});

test('ordering is numeric, not lexicographic', () => {
  const releases = [product('v0.9.0', '0.9.0'), product('v0.10.0', '0.10.0')];
  assert.equal(latestProductTag(releases), 'v0.10.0');
  assert.equal(latestProductTag([...releases].reverse()), 'v0.10.0');
  assert.equal(latestProductTag([product('v1.0.0', '1.0.0'), ...LIVE_RELEASES]), 'v1.0.0');
});

test('a valid pin overrides discovery', () => {
  assert.equal(resolveAdvertisedTag('v0.4.6', LIVE_RELEASES), 'v0.4.6');
  assert.equal(resolveAdvertisedTag('  v0.4.6  ', LIVE_RELEASES), 'v0.4.6');
  // A pin written without the leading v still resolves to the real tag.
  assert.equal(resolveAdvertisedTag('0.4.6', LIVE_RELEASES), 'v0.4.6');
});

test('a prerelease pin is honored, but is never discovered', () => {
  // site-pin.yml accepts vX.Y.Z-rc.N, so a pin to one has to work; discovery
  // must still skip it.
  const rc = { ...product('v0.5.0-rc.1', '0.5.0-rc.1'), prerelease: true };
  const withRc = [rc, ...LIVE_RELEASES];
  assert.equal(resolveAdvertisedTag('v0.5.0-rc.1', withRc), 'v0.5.0-rc.1');
  assert.equal(resolveAdvertisedTag('', withRc), 'v0.4.7');
});

test('an unusable pin falls through to discovery', () => {
  assert.equal(resolveAdvertisedTag('v9.9.9', LIVE_RELEASES), 'v0.4.7');
  assert.equal(resolveAdvertisedTag('guest-image-v2026.08.05', LIVE_RELEASES), 'v0.4.7');
  // Present, well-named, but nothing installable behind it.
  const hollow = { tag_name: 'v0.4.8', draft: false, prerelease: false, assets: [] };
  assert.equal(resolveAdvertisedTag('v0.4.8', [hollow, ...LIVE_RELEASES]), 'v0.4.7');
});

test('a pin is honored when the release list could not be fetched', () => {
  assert.equal(resolveAdvertisedTag('v0.4.2', null), 'v0.4.2');
});

test('no resolvable release falls back to no version', () => {
  assert.equal(resolveAdvertisedTag('', null), null);
  assert.equal(resolveAdvertisedTag('', []), null);
  assert.equal(resolveAdvertisedTag('', [guestImage('guest-image-v2026.08.05')]), null);
  assert.equal(versionFromTag(null), null);
});
