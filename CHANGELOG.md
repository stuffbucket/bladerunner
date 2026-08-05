# Changelog

## [0.4.8](https://github.com/stuffbucket/bladerunner/compare/v0.4.7...v0.4.8) (2026-08-05)


### Features

* **cartridge:** persist a booted .dmg on eject ([#189](https://github.com/stuffbucket/bladerunner/issues/189)) ([bad09f1](https://github.com/stuffbucket/bladerunner/commit/bad09f19edc181b23022e95265b03399154cdfb3))
* **cli:** add br up + grouped help; dedup --json guard ([#146](https://github.com/stuffbucket/bladerunner/issues/146)) ([0f7cc73](https://github.com/stuffbucket/bladerunner/commit/0f7cc73e233625d7818af76b91f40cf7569af399)), closes [#131](https://github.com/stuffbucket/bladerunner/issues/131)
* **cli:** answer to the verbs other tools use ([#273](https://github.com/stuffbucket/bladerunner/issues/273)) ([73b637c](https://github.com/stuffbucket/bladerunner/commit/73b637ce2029a49092feace6bad97b285ff67edb))
* **cli:** close the ergonomics gap with colima ([#271](https://github.com/stuffbucket/bladerunner/issues/271)) ([b92ee59](https://github.com/stuffbucket/bladerunner/commit/b92ee59ef7e475e525a4ac0b30804d097de905e1))
* **cli:** run the VM under the holder ([#190](https://github.com/stuffbucket/bladerunner/issues/190)) ([54cb184](https://github.com/stuffbucket/bladerunner/commit/54cb18447daa6412b17b371f404775e573b9be06))
* **cli:** suggest the right verb for another tool's word ([#272](https://github.com/stuffbucket/bladerunner/issues/272)) ([c688722](https://github.com/stuffbucket/bladerunner/commit/c688722d2cd0bc7ddae56d506b374a60dde347b4))
* **cli:** surface cartridge eject protection ([#187](https://github.com/stuffbucket/bladerunner/issues/187)) ([5c0b623](https://github.com/stuffbucket/bladerunner/commit/5c0b623bdf260f2ba32397df856e8f06d50dcd12))
* default to the pre-baked guest image ([#173](https://github.com/stuffbucket/bladerunner/issues/173)) ([ce330e8](https://github.com/stuffbucket/bladerunner/commit/ce330e8e7cdfa07221d4073cae0f81e5bd582d7a))
* enable + fail-closed-verify the hosted image ([#172](https://github.com/stuffbucket/bladerunner/issues/172)) ([fb81338](https://github.com/stuffbucket/bladerunner/commit/fb81338f32a8b7313c07061a3bfbbcd0f8f4df04)), closes [#155](https://github.com/stuffbucket/bladerunner/issues/155)
* **imagebuild:** add the Linux native build mechanic ([#255](https://github.com/stuffbucket/bladerunner/issues/255)) ([bb90d29](https://github.com/stuffbucket/bladerunner/commit/bb90d29852e2945c6e006b5548a4b8fc0498a973))
* **imagebuild:** create the Incus storage pool at bake time ([#275](https://github.com/stuffbucket/bladerunner/issues/275)) ([b991811](https://github.com/stuffbucket/bladerunner/commit/b9918111a266758e6833b328e3c5c0ea91536f93))
* **imagebuild:** fetch a pinned, verified base image ([#259](https://github.com/stuffbucket/bladerunner/issues/259)) ([759ac33](https://github.com/stuffbucket/bladerunner/commit/759ac33272681882d1d33060905be3b484374e3e))
* **imagebuild:** own guest image build policy ([#250](https://github.com/stuffbucket/bladerunner/issues/250)) ([ca37cf1](https://github.com/stuffbucket/bladerunner/commit/ca37cf1391314aa1722dd9b3af30ee2a561f36df))
* **imagebuild:** own the guest image build in Go ([#263](https://github.com/stuffbucket/bladerunner/issues/263)) ([2918848](https://github.com/stuffbucket/bladerunner/commit/2918848bdd5f85c2e7bc42ba979ae2c3fb5f9e2e))
* **update:** ed25519 self-updater + .dmg docs ([#148](https://github.com/stuffbucket/bladerunner/issues/148)) ([cd3e0c1](https://github.com/stuffbucket/bladerunner/commit/cd3e0c1f829d9ec6b5d479da5039bc13c95b217e))
* **update:** embed production update public key ([#150](https://github.com/stuffbucket/bladerunner/issues/150)) ([9bd25fd](https://github.com/stuffbucket/bladerunner/commit/9bd25fdcfc99b735dfb1441b8203c6082cb4e028))
* **update:** publish latest.json on release ([#151](https://github.com/stuffbucket/bladerunner/issues/151)) ([09c1332](https://github.com/stuffbucket/bladerunner/commit/09c1332fdb603ec4fc4b1da94bc89be61f38de0d))
* **vm:** share one base image across instances ([#274](https://github.com/stuffbucket/bladerunner/issues/274)) ([04600c5](https://github.com/stuffbucket/bladerunner/commit/04600c56f98df7afdd08331b9431765cdde48b15))


### Bug Fixes

* answer a nil handler response; refresh site copy ([#293](https://github.com/stuffbucket/bladerunner/issues/293)) ([4387300](https://github.com/stuffbucket/bladerunner/commit/4387300de1f700e1b84ab0cc82501ca9ebdc2cec)), closes [#230](https://github.com/stuffbucket/bladerunner/issues/230)
* bind OIDC codes; refuse archive symlink escapes ([#300](https://github.com/stuffbucket/bladerunner/issues/300)) ([28ce516](https://github.com/stuffbucket/bladerunner/commit/28ce51698856955d6d4acc252655737e76377155))
* **build:** verify the Go toolchain for signed builds ([#256](https://github.com/stuffbucket/bladerunner/issues/256)) ([3c16131](https://github.com/stuffbucket/bladerunner/commit/3c1613176ddfc2b001a815b9a7f509c91a821e2b))
* **cartridge:** never unlink an unconfirmed attachment ([#296](https://github.com/stuffbucket/bladerunner/issues/296)) ([d15c326](https://github.com/stuffbucket/bladerunner/commit/d15c3269cbb94fe5f0b7ab1b520a754dd5cbd9c9))
* **ci:** inline the triage workflow ([#251](https://github.com/stuffbucket/bladerunner/issues/251)) ([586a36e](https://github.com/stuffbucket/bladerunner/commit/586a36e4e5c800134e641c9fcb7456fa9b3b18ea))
* **ci:** stop passing a flag that no longer exists ([#267](https://github.com/stuffbucket/bladerunner/issues/267)) ([ae67e45](https://github.com/stuffbucket/bladerunner/commit/ae67e455a01c2af9c81da77a14bc4c2aa6eaa76b))
* **cli:** correct the user-facing command surface ([#245](https://github.com/stuffbucket/bladerunner/issues/245)) ([31454d8](https://github.com/stuffbucket/bladerunner/commit/31454d8fe6a781ed46d9b991dc9dbbd2f955db54))
* close the last audit findings ([#191](https://github.com/stuffbucket/bladerunner/issues/191)) ([8018c32](https://github.com/stuffbucket/bladerunner/commit/8018c326aefb4185464bddff5d72d4b1c8ee7528))
* **disk:** take the bake digest only from stdout ([#260](https://github.com/stuffbucket/bladerunner/issues/260)) ([a4ba093](https://github.com/stuffbucket/bladerunner/commit/a4ba093f7c48b1b5c6d037033f96613d3acaabe5))
* **imagebuild:** load nbd before answering about it ([#269](https://github.com/stuffbucket/bladerunner/issues/269)) ([836bb87](https://github.com/stuffbucket/bladerunner/commit/836bb876fec3aa9ffdc5a8f3172817e018a89c02))
* **imagebuild:** make the probe describe what runs ([#253](https://github.com/stuffbucket/bladerunner/issues/253)) ([c4d5f98](https://github.com/stuffbucket/bladerunner/commit/c4d5f9836748f6c69650e7482bfaf22537c1b14b))
* **imagebuild:** validate a GPT before trusting it ([#270](https://github.com/stuffbucket/bladerunner/issues/270)) ([d48b27c](https://github.com/stuffbucket/bladerunner/commit/d48b27c0c4c7a7135c34e0a5ad7ff812aaf15cd2))
* **image:** never delete a caller-owned work directory ([#254](https://github.com/stuffbucket/bladerunner/issues/254)) ([133cfef](https://github.com/stuffbucket/bladerunner/commit/133cfef0eb1b629305c0b4760da512e90a2dab82))
* **image:** streamline the Incus base image ([#57](https://github.com/stuffbucket/bladerunner/issues/57), [#45](https://github.com/stuffbucket/bladerunner/issues/45)) ([#125](https://github.com/stuffbucket/bladerunner/issues/125)) ([29d0a8c](https://github.com/stuffbucket/bladerunner/commit/29d0a8cc462aa833fd48db1c7a8ba6a78bc5b786))
* keep saved state and its sidecar together ([#292](https://github.com/stuffbucket/bladerunner/issues/292)) ([efb0357](https://github.com/stuffbucket/bladerunner/commit/efb0357aeae4b20b2effae0fc273ec5efef6a733))
* **lint:** drop stale nolint directives; pin golangci ([#149](https://github.com/stuffbucket/bladerunner/issues/149)) ([8e60abf](https://github.com/stuffbucket/bladerunner/commit/8e60abfa29314c2def794ccc6405366738462919))
* **lint:** resolve golangci-lint failures blocking main ([#128](https://github.com/stuffbucket/bladerunner/issues/128)) ([477117d](https://github.com/stuffbucket/bladerunner/commit/477117d27c4b4f608d893d06ab7194c49a2867e5))
* make --instance actually select the instance ([#183](https://github.com/stuffbucket/bladerunner/issues/183)) ([4124752](https://github.com/stuffbucket/bladerunner/commit/41247522b0e7814a627633b9eb5ee9556ceba005))
* make br exec interruptible ([#301](https://github.com/stuffbucket/bladerunner/issues/301)) ([defe286](https://github.com/stuffbucket/bladerunner/commit/defe286bda33945c2ac96625c2cf20f339611879)), closes [#283](https://github.com/stuffbucket/bladerunner/issues/283)
* make the cartridge smoke test pass end to end ([#177](https://github.com/stuffbucket/bladerunner/issues/177)) ([a059ac7](https://github.com/stuffbucket/bladerunner/commit/a059ac7cc6ab2acd080bc3290434ab8a85241540))
* **provision:** close agent-path OIDC relay gap ([#130](https://github.com/stuffbucket/bladerunner/issues/130)) ([#145](https://github.com/stuffbucket/bladerunner/issues/145)) ([95403b0](https://github.com/stuffbucket/bladerunner/commit/95403b0fa9776d0d5a39f211ccbeaa84b0c443c8))
* **provision:** dedicated ssh user, no incus collision ([#174](https://github.com/stuffbucket/bladerunner/issues/174)) ([f7f2548](https://github.com/stuffbucket/bladerunner/commit/f7f254842ed1fec168bda29da9eef2621c368f82))
* **provision:** stop blocking first boot on a backend that comes later ([#276](https://github.com/stuffbucket/bladerunner/issues/276)) ([bd3ef85](https://github.com/stuffbucket/bladerunner/commit/bd3ef8540f43574e6907c4ffec0d029f672978e0))
* **provision:** stop reinstalling what the image already has ([#277](https://github.com/stuffbucket/bladerunner/issues/277)) ([fbb6459](https://github.com/stuffbucket/bladerunner/commit/fbb6459dba44932e4020c36f95385ec73a647535))
* **provision:** tie the instance-id to the user-data ([#257](https://github.com/stuffbucket/bladerunner/issues/257)) ([4e6af0c](https://github.com/stuffbucket/bladerunner/commit/4e6af0c686e2536ba3f0667f31c3cd06aff2eeb4))
* publish JSON manifests atomically ([#184](https://github.com/stuffbucket/bladerunner/issues/184)) ([2124101](https://github.com/stuffbucket/bladerunner/commit/212410188e151faf7e2fa1b287ed3b7bef828176))
* publish the seed atomically; report skipped steps ([#298](https://github.com/stuffbucket/bladerunner/issues/298)) ([11600c0](https://github.com/stuffbucket/bladerunner/commit/11600c05d281fff244a93c7d741dfc3c2476a9db))
* recover a wedged holder and stop the accept spin ([#289](https://github.com/stuffbucket/bladerunner/issues/289)) ([c947f35](https://github.com/stuffbucket/bladerunner/commit/c947f35b9b1d7cdaa6080ce8c9bd71a7ee328b9c))
* serialize the image cache, rename safely ([#291](https://github.com/stuffbucket/bladerunner/issues/291)) ([3c30238](https://github.com/stuffbucket/bladerunner/commit/3c3023816231f4e5efbcb74516dbf3ade4c9f334)), closes [#280](https://github.com/stuffbucket/bladerunner/issues/280) [#281](https://github.com/stuffbucket/bladerunner/issues/281)
* **site:** advertise the product release, not a guest image ([#295](https://github.com/stuffbucket/bladerunner/issues/295)) ([27539cd](https://github.com/stuffbucket/bladerunner/commit/27539cd7d073bd4ae82fe24fe41a5a4214676b8b)), closes [#226](https://github.com/stuffbucket/bladerunner/issues/226)
* **ssh:** derive the public key, publish atomically ([#297](https://github.com/stuffbucket/bladerunner/issues/297)) ([9f42c46](https://github.com/stuffbucket/bladerunner/commit/9f42c4612e874007b342e13615df5b540dbbae08)), closes [#214](https://github.com/stuffbucket/bladerunner/issues/214)
* stamp the binary from a product tag ([#299](https://github.com/stuffbucket/bladerunner/issues/299)) ([3f009c4](https://github.com/stuffbucket/bladerunner/commit/3f009c4c46101eb17ebd09b38f827010a92b98ce)), closes [#294](https://github.com/stuffbucket/bladerunner/issues/294)
* **test:** make smoke cleanup interruption-safe ([#302](https://github.com/stuffbucket/bladerunner/issues/302)) ([232f9d3](https://github.com/stuffbucket/bladerunner/commit/232f9d3470c86ade2fb35c97ad1d8856f76d07af)), closes [#228](https://github.com/stuffbucket/bladerunner/issues/228)
* **update:** derive latest.json at build time ([#303](https://github.com/stuffbucket/bladerunner/issues/303)) ([d2d6bda](https://github.com/stuffbucket/bladerunner/commit/d2d6bdad9349df8178f1dab70ecdc5d915190a7f)), closes [#232](https://github.com/stuffbucket/bladerunner/issues/232) [#286](https://github.com/stuffbucket/bladerunner/issues/286)
* **vmhost:** publish startedAt under the lock ([#238](https://github.com/stuffbucket/bladerunner/issues/238)) ([09d30b1](https://github.com/stuffbucket/bladerunner/commit/09d30b1588d4902084ae83b9797450cf434fe783))

## [0.4.7](https://github.com/stuffbucket/bladerunner/compare/v0.4.6...v0.4.7) (2026-06-22)


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
* **ui:** splash re-palette + four-slash app icon polish ([#121](https://github.com/stuffbucket/bladerunner/issues/121)) ([f08b2ed](https://github.com/stuffbucket/bladerunner/commit/f08b2ed921b3bf49fe89bf311d4addcd9082e983))
* **ui:** web proxy, console-off default, live splash + brand refresh ([#124](https://github.com/stuffbucket/bladerunner/issues/124)) ([8a4fbae](https://github.com/stuffbucket/bladerunner/commit/8a4fbaeca6f8e725598dbe360bff2b46b1ef355d))

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
