# Changelog

## [0.4.3](https://github.com/stuffbucket/bladerunner/compare/v0.4.2...v0.4.3) (2026-06-09)


### Features

* **site:** advertise the latest version + pin/unpin (maximal lesson) ([#94](https://github.com/stuffbucket/bladerunner/issues/94)) ([0a46fe4](https://github.com/stuffbucket/bladerunner/commit/0a46fe411f7127773ea6e59fe9fecc14cbd589dc))

## [0.4.2](https://github.com/stuffbucket/bladerunner/compare/v0.4.1...v0.4.2) (2026-06-09)


### Features

* **release:** signed + notarized Bladerunner.app DMG + cask ([#91](https://github.com/stuffbucket/bladerunner/issues/91)) ([0a7c02d](https://github.com/stuffbucket/bladerunner/commit/0a7c02dc3f263820c7d44333e32bfb96c046472d))

## [0.4.1](https://github.com/stuffbucket/bladerunner/compare/v0.4.0...v0.4.1) (2026-06-09)


### Bug Fixes

* **ci:** move Homebrew formula template out of ignored build/ ([#88](https://github.com/stuffbucket/bladerunner/issues/88)) ([4c983ee](https://github.com/stuffbucket/bladerunner/commit/4c983eeb5f9fc5e603774066ad35dd08fb7d3b8b))

## [0.4.0](https://github.com/stuffbucket/bladerunner/compare/v0.3.0...v0.4.0) (2026-06-09)


### ⚠ BREAKING CHANGES

* **cli:** the CLI command is now br (was runner). The next release ships the br binary; the Homebrew formula installs it as br.

### Features

* **cartridge:** AirDrop-able DMG cartridges — boot/eject a whole VM as one file ([#72](https://github.com/stuffbucket/bladerunner/issues/72)) ([0f92a99](https://github.com/stuffbucket/bladerunner/commit/0f92a99f83fa77b85c7cc4472bcac9a50200d033))
* **cli:** rename command runner to br ([#84](https://github.com/stuffbucket/bladerunner/issues/84)) ([977e1b6](https://github.com/stuffbucket/bladerunner/commit/977e1b6c6b9c31e38ffd957667dde795249f5606))
* **disk:** bootable disks — slide-in image+config bundles (boot/eject/disks/disk verbs) ([#71](https://github.com/stuffbucket/bladerunner/issues/71)) ([0161228](https://github.com/stuffbucket/bladerunner/commit/01612281f56b0ddfaf3648457f6784ac8847a185))
* **guest:** collapse watchdog clock heal to an instant host re-measure ([#81](https://github.com/stuffbucket/bladerunner/issues/81)) ([1518cf7](https://github.com/stuffbucket/bladerunner/commit/1518cf77c4b327f13659459ab49a5bd1ebcca2d7))
* **menubar:** macOS menubar app mirroring the core CLI ([#76](https://github.com/stuffbucket/bladerunner/issues/76)) ([394d61f](https://github.com/stuffbucket/bladerunner/commit/394d61f5ae6a976f5f1a445df15cfb45b967df3f))
* **site:** Caddy-inspired Astro landing page ([#62](https://github.com/stuffbucket/bladerunner/issues/62)) ([fb931ba](https://github.com/stuffbucket/bladerunner/commit/fb931ba9ee8b7dc7a129eda7dddc3a3ba0650e8c))
* **time:** guest clock resilience across host sleep — host pseudo-NTP over vsock + chrony + watchdog ([#78](https://github.com/stuffbucket/bladerunner/issues/78)) ([5c939d0](https://github.com/stuffbucket/bladerunner/commit/5c939d0d1a5b6da4bc916fd696aa0c6aed69e110))
* **ui:** SVG banner + stylized 'br' dock & menubar icons ([#82](https://github.com/stuffbucket/bladerunner/issues/82)) ([340ed56](https://github.com/stuffbucket/bladerunner/commit/340ed563213c7607d6c3bac4d307d69b72dde25b))


### Bug Fixes

* **boot:** extend default WaitForIncus to 10m + stream cloud-init breadcrumbs ([#58](https://github.com/stuffbucket/bladerunner/issues/58)) ([f13b4c3](https://github.com/stuffbucket/bladerunner/commit/f13b4c30caa5b785c8c3d8197dd3f60d3e9c1ebf)), closes [#52](https://github.com/stuffbucket/bladerunner/issues/52)
* **reconnect:** restart chrony + vsock-ntp, not the now-masked timesyncd ([#80](https://github.com/stuffbucket/bladerunner/issues/80)) ([5030b92](https://github.com/stuffbucket/bladerunner/commit/5030b9258863452cd5e2438f503c88d02ef112dc))
