# Changelog

## [0.4.7](https://github.com/stuffbucket/bladerunner/compare/v0.4.6...v0.4.7) (2026-06-11)


### Features

* **config:** persisted closed-union Settings layer (menubar-app 1/N) ([#105](https://github.com/stuffbucket/bladerunner/issues/105)) ([9ce2591](https://github.com/stuffbucket/bladerunner/commit/9ce25916dc3499a8d49a259e991bd424f5ca2b87))
* **menubar:** branded UNUserNotificationCenter banners ([#110](https://github.com/stuffbucket/bladerunner/issues/110)) ([7447360](https://github.com/stuffbucket/bladerunner/commit/744736050d0dddf4bb1a4f0ce1ead4ae6f5cefb0))
* **menubar:** cgo splash window (floating HUD) ([#109](https://github.com/stuffbucket/bladerunner/issues/109)) ([9ac1198](https://github.com/stuffbucket/bladerunner/commit/9ac1198f27447bafa65ddf5f4bec01fe0d993070))
* **menubar:** edge-triggered VM-state notification state machine ([#107](https://github.com/stuffbucket/bladerunner/issues/107)) ([3f55c43](https://github.com/stuffbucket/bladerunner/commit/3f55c43878da6d808bb2fae227da64c3768d9259))
* **menubar:** settings window (WKWebView form over closed-union config) ([#112](https://github.com/stuffbucket/bladerunner/issues/112)) ([f510a86](https://github.com/stuffbucket/bladerunner/commit/f510a863e2d43900b22b606c673d90403fca2c14))
* **menubar:** single-instance guard with present-handoff socket ([#108](https://github.com/stuffbucket/bladerunner/issues/108)) ([20be2c6](https://github.com/stuffbucket/bladerunner/commit/20be2c6435d8ff0d3d176221ed61a23cadcf600e))
* **menubar:** version-aware single-instance handoff + engine-update surfacing ([#115](https://github.com/stuffbucket/bladerunner/issues/115)) ([edea5f2](https://github.com/stuffbucket/bladerunner/commit/edea5f27a5849d75c720c8b01496c8cf0dc12896))
* **menubar:** wire start-VM policy (manual/on-launch/on-first-action) ([#111](https://github.com/stuffbucket/bladerunner/issues/111)) ([87ee45b](https://github.com/stuffbucket/bladerunner/commit/87ee45b83eea7e82d5a2988b628955a3751d3c41))
* **start:** overlay persisted Settings into start reconciliation ([#106](https://github.com/stuffbucket/bladerunner/issues/106)) ([5d6fe15](https://github.com/stuffbucket/bladerunner/commit/5d6fe1533fabf28ff49ce4474f92c92c08be9be7))
* **ui:** banner-driven shimmer splash + clean 'br' icons ([#114](https://github.com/stuffbucket/bladerunner/issues/114)) ([fe79b82](https://github.com/stuffbucket/bladerunner/commit/fe79b822001c228d66606bb0060b8a9a4c6a0cea))
* **ui:** redo dock + menubar icons in the banner's slant style ([#103](https://github.com/stuffbucket/bladerunner/issues/103)) ([479aeca](https://github.com/stuffbucket/bladerunner/commit/479aeca8ecb9e845483beb6ee75108a55bab952a))
* **ui:** shrink the CLI banner from 'bladerunner' to 'br' ([#113](https://github.com/stuffbucket/bladerunner/issues/113)) ([518ff6f](https://github.com/stuffbucket/bladerunner/commit/518ff6f51d856a8e4223a4ecbd6c9eb2abef76a0))

## [0.4.6](https://github.com/stuffbucket/bladerunner/compare/v0.4.5...v0.4.6) (2026-06-10)


### Features

* **release:** request a semver-arch DMG filename from macos-builder ([#101](https://github.com/stuffbucket/bladerunner/issues/101)) ([1b9912c](https://github.com/stuffbucket/bladerunner/commit/1b9912c3c1bc996ee4a993c9e284d574374f6aef))

## [0.4.5](https://github.com/stuffbucket/bladerunner/compare/v0.4.4...v0.4.5) (2026-06-09)


### Bug Fixes

* **macos-builder:** provision Go in the producer (runner has bun/cargo, not Go) ([#99](https://github.com/stuffbucket/bladerunner/issues/99)) ([26845c2](https://github.com/stuffbucket/bladerunner/commit/26845c2a1dbdba0e2724d4ec22b3a0a4da8e7e5d))

## [0.4.4](https://github.com/stuffbucket/bladerunner/compare/v0.4.3...v0.4.4) (2026-06-09)


### Features

* **release:** prepare bladerunner as a macos-builder client ([#97](https://github.com/stuffbucket/bladerunner/issues/97)) ([1162815](https://github.com/stuffbucket/bladerunner/commit/11628150b1704b2f1aa9fad9f30a686f3ffcf757))

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
