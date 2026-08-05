// Which release the landing page advertises.
//
// This repository publishes TWO families of release from one tag namespace:
// the product (`vX.Y.Z`, carrying the CLI/app artifacts) and the guest image
// (`guest-image-vYYYY.MM.DD`, carrying only qcow2 files). GitHub's
// /releases/latest marker is repo-wide, so it points at whichever family
// published last — which is how the site came to advertise a guest-image build
// date as the CLI version and link its download CTA at a release with no CLI in
// it. Selection therefore has to be an explicit contract, not GitHub's marker.
//
// WHY NOT latest.json: site/public/latest.json is the self-update manifest, and
// it is a tempting source of truth until you look at when it exists. It is
// written only after stuffbucket/macos-builder uploads a SIGNED updater tarball
// (.github/workflows/publish-update-manifest.yml polls for it and exits 0 with a
// notice when it never arrives), so it is absent from the tree today and a site
// build that depended on it would show no version at all. It also carries a bare
// `version`, not a tag, so the release CTA would have to reconstruct a tag that
// may not exist, and it is committed INTO the site build — the page would
// advertise what was last committed rather than what is published. The releases
// list is the published state, and it carries the asset names needed to prove a
// CLI artifact is actually there.
//
// Plain JavaScript with JSDoc types, not TypeScript, so `node --test` runs the
// unit test beside it with no compiler, no flags and no new dependency.

/**
 * @typedef {{name?: unknown}} ReleaseAsset
 * @typedef {{tag_name?: unknown, draft?: unknown, prerelease?: unknown, assets?: unknown}} Release
 */

/** Tag shape of a product release. `guest-image-v2026.08.05` does not match. */
export const PRODUCT_TAG = /^v\d+\.\d+\.\d+$/;

/**
 * Asset-name shape of a shipped CLI/app artifact: the `.dmg` installer or the
 * `.tar.gz` archive. A guest image's `*.qcow2` and any `*.sha256` sidecar do
 * not match, so a release has to carry something installable to be advertised.
 */
const CLI_ASSET = /^bladerunner[._-]?.*\.(dmg|tar\.gz)$/i;

/** Number of tag components compared by semver ordering: major, minor, patch. */
const SEMVER_PARTS = 3;

/**
 * isProductTag reports whether a tag names a product release.
 * @param {unknown} tag
 * @returns {boolean}
 */
export function isProductTag(tag) {
  return typeof tag === 'string' && PRODUCT_TAG.test(tag);
}

/**
 * hasCliAsset reports whether a release actually carries an installable CLI/app
 * artifact, rather than only checksums or guest-image disks.
 * @param {Release} release
 * @returns {boolean}
 */
export function hasCliAsset(release) {
  const assets = release && Array.isArray(release.assets) ? release.assets : [];
  return assets.some((asset) => {
    const name = asset ? /** @type {ReleaseAsset} */ (asset).name : undefined;
    return typeof name === 'string' && CLI_ASSET.test(name);
  });
}

/**
 * isAdvertisableRelease reports whether a release is published and carries an
 * installable CLI/app artifact. This is the weaker test the PIN is held to: a
 * pin is a deliberate human choice, and site-pin.yml accepts a prerelease tag
 * (`vX.Y.Z-rc.1`), so requiring the strict discovery contract here would
 * silently ignore a pin the operator was allowed to set.
 * @param {Release} release
 * @returns {boolean}
 */
export function isAdvertisableRelease(release) {
  if (!release || release.draft === true) return false;
  return hasCliAsset(release);
}

/**
 * isProductRelease reports whether a release entry from GitHub's /releases list
 * is a published product release the site may advertise ON ITS OWN: not a
 * draft, not a prerelease, a strict `vX.Y.Z` tag, and carrying a CLI asset.
 * @param {Release} release
 * @returns {boolean}
 */
export function isProductRelease(release) {
  if (!release || release.prerelease === true) return false;
  return isProductTag(release.tag_name) && isAdvertisableRelease(release);
}

/**
 * compareTagsDescending orders two product tags newest-first by NUMERIC semver.
 * String order would put v0.9.0 above v0.10.0.
 * @param {string} a
 * @param {string} b
 * @returns {number}
 */
function compareTagsDescending(a, b) {
  const left = a.slice(1).split('.').map(Number);
  const right = b.slice(1).split('.').map(Number);
  for (let i = 0; i < SEMVER_PARTS; i++) {
    if (left[i] !== right[i]) return right[i] - left[i];
  }
  return 0;
}

/**
 * latestProductTag picks the newest product tag out of a /releases list. It does
 * not trust the list's order. Returns null when the list holds no product
 * release, which is the site's no-version fallback.
 * @param {unknown} releases
 * @returns {string | null}
 */
export function latestProductTag(releases) {
  if (!Array.isArray(releases)) return null;
  const tags = releases
    .filter((release) => isProductRelease(release))
    .map((release) => /** @type {string} */ (release.tag_name));
  if (tags.length === 0) return null;
  return tags.sort(compareTagsDescending)[0];
}

/**
 * resolveAdvertisedTag returns the tag the page should advertise, or null.
 *
 * A pin (the SITE_PIN_VERSION repo variable) still wins, but it now has to name
 * a published release that carries a CLI asset: a stale, mistyped or
 * guest-image pin falls through to discovery instead of advertising a tag with
 * nothing behind it. When the release list could not be fetched at all (null)
 * the pin is honored as written, because a human set it and there is nothing to
 * check it against.
 *
 * @param {string | null | undefined} pinnedTag
 * @param {unknown} releases /releases list, or null when the lookup failed
 * @returns {string | null}
 */
export function resolveAdvertisedTag(pinnedTag, releases) {
  const pin = (pinnedTag ?? '').trim();
  if (pin) {
    if (!Array.isArray(releases)) return pin;
    const wanted = pin.startsWith('v') ? [pin] : [pin, `v${pin}`];
    const hit = releases.find((release) => {
      if (!release) return false;
      const tag = /** @type {Release} */ (release).tag_name;
      return typeof tag === 'string' && wanted.includes(tag) && isAdvertisableRelease(release);
    });
    if (hit) return /** @type {string} */ (/** @type {Release} */ (hit).tag_name);
  }
  return latestProductTag(releases);
}

/**
 * versionFromTag renders a tag as the version chip's text.
 * @param {string | null} tag
 * @returns {string | null}
 */
export function versionFromTag(tag) {
  return typeof tag === 'string' && tag.length > 0 ? tag.replace(/^v/, '') : null;
}
