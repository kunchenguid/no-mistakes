# Changelog

## [1.12.0](https://github.com/kunchenguid/no-mistakes/compare/v1.11.0...v1.12.0) (2026-05-03)


### Features

* **agent:** add ACP target support via acpx ([#165](https://github.com/kunchenguid/no-mistakes/issues/165)) ([e6db093](https://github.com/kunchenguid/no-mistakes/commit/e6db09381ca1d64725427006ea960bc1ea827b15))

## [1.11.0](https://github.com/kunchenguid/no-mistakes/compare/v1.10.8...v1.11.0) (2026-04-30)


### Features

* **agent:** add Pi agent support ([#161](https://github.com/kunchenguid/no-mistakes/issues/161)) ([9048a93](https://github.com/kunchenguid/no-mistakes/commit/9048a934651ba784aff44d0727ae5142329bb143))

## [1.10.8](https://github.com/kunchenguid/no-mistakes/compare/v1.10.7...v1.10.8) (2026-04-27)


### Bug Fixes

* **agent:** retry transient invocation failures ([#159](https://github.com/kunchenguid/no-mistakes/issues/159)) ([ebdaac7](https://github.com/kunchenguid/no-mistakes/commit/ebdaac7afaddf88f77deaf0d21dce3e3c35248ea))

## [1.10.7](https://github.com/kunchenguid/no-mistakes/compare/v1.10.6...v1.10.7) (2026-04-27)


### Bug Fixes

* **telemetry:** use self-hosted Umami defaults ([#157](https://github.com/kunchenguid/no-mistakes/issues/157)) ([efef934](https://github.com/kunchenguid/no-mistakes/commit/efef934ba9ec451ac3389f2f069caa9e6bf43287))

## [1.10.6](https://github.com/kunchenguid/no-mistakes/compare/v1.10.5...v1.10.6) (2026-04-27)


### Bug Fixes

* **telemetry:** reduce event data volume ([#155](https://github.com/kunchenguid/no-mistakes/issues/155)) ([0b8c6a0](https://github.com/kunchenguid/no-mistakes/commit/0b8c6a036ed09244ce04cad870c3cdaf296d022f))

## [1.10.5](https://github.com/kunchenguid/no-mistakes/compare/v1.10.4...v1.10.5) (2026-04-25)


### Bug Fixes

* **update:** resolve beta releases from tags ([#152](https://github.com/kunchenguid/no-mistakes/issues/152)) ([e2558ba](https://github.com/kunchenguid/no-mistakes/commit/e2558baeb9761160a6966844689808587b7fbeeb))

## [1.10.4](https://github.com/kunchenguid/no-mistakes/compare/v1.10.3...v1.10.4) (2026-04-25)


### Bug Fixes

* **daemon:** retry busy launchctl bootstrap ([#150](https://github.com/kunchenguid/no-mistakes/issues/150)) ([40412a8](https://github.com/kunchenguid/no-mistakes/commit/40412a871bcff44802430b9d8406f1b67d945f1f))

## [1.10.3](https://github.com/kunchenguid/no-mistakes/compare/v1.10.2...v1.10.3) (2026-04-24)


### Bug Fixes

* **daemon:** refresh managed services with stable PATH ([#148](https://github.com/kunchenguid/no-mistakes/issues/148)) ([cac217f](https://github.com/kunchenguid/no-mistakes/commit/cac217f6a931bd0afcde6379cc310bf995285464))

## [1.10.2](https://github.com/kunchenguid/no-mistakes/compare/v1.10.1...v1.10.2) (2026-04-24)


### Bug Fixes

* **agent:** harden structured JSON fallback parsing ([#144](https://github.com/kunchenguid/no-mistakes/issues/144)) ([21449db](https://github.com/kunchenguid/no-mistakes/commit/21449dbdcffabe35bc6c34b4c39fdb6ba2a5fae4))
* **pipeline:** make CI auto-fix retry after reruns reliably ([#145](https://github.com/kunchenguid/no-mistakes/issues/145)) ([e7320c7](https://github.com/kunchenguid/no-mistakes/commit/e7320c70135df494daeb0fe61a41001de46f3e92))

## [1.10.1](https://github.com/kunchenguid/no-mistakes/compare/v1.10.0...v1.10.1) (2026-04-23)


### Bug Fixes

* **agent:** support Codex output schema parsing ([#141](https://github.com/kunchenguid/no-mistakes/issues/141)) ([f4f253e](https://github.com/kunchenguid/no-mistakes/commit/f4f253efdc3eeba34ad852d97f791ee7a034b060))

## [1.10.0](https://github.com/kunchenguid/no-mistakes/compare/v1.9.2...v1.10.0) (2026-04-22)


### Features

* **agent:** support agent args override ([#133](https://github.com/kunchenguid/no-mistakes/issues/133)) ([18c58ae](https://github.com/kunchenguid/no-mistakes/commit/18c58aef526a7c2b887246e80c9455bbebe27811))
* **tui:** add finding overrides for manual fix rounds ([#135](https://github.com/kunchenguid/no-mistakes/issues/135)) ([4b62714](https://github.com/kunchenguid/no-mistakes/commit/4b62714cb55be3a09fe1acd1da1532dea39d2583))

## [1.9.2](https://github.com/kunchenguid/no-mistakes/compare/v1.9.1...v1.9.2) (2026-04-22)


### Bug Fixes

* **git:** move core.bare into worktree config ([#130](https://github.com/kunchenguid/no-mistakes/issues/130)) ([75f20a9](https://github.com/kunchenguid/no-mistakes/commit/75f20a9027732ad121244a174f44f1a312b9f9d7))

## [1.9.1](https://github.com/kunchenguid/no-mistakes/compare/v1.9.0...v1.9.1) (2026-04-21)


### Bug Fixes

* **daemon:** clean up timed-out daemon startups safely ([#128](https://github.com/kunchenguid/no-mistakes/issues/128)) ([b6a2389](https://github.com/kunchenguid/no-mistakes/commit/b6a238921439347e377ed6ed46f4716e95830b6b))
* **git:** isolate gate hooks from shared hookspath changes ([#127](https://github.com/kunchenguid/no-mistakes/issues/127)) ([a1d5bab](https://github.com/kunchenguid/no-mistakes/commit/a1d5bab0631ae443d9a83943527613b743e95e3b))

## [1.9.0](https://github.com/kunchenguid/no-mistakes/compare/v1.8.1...v1.9.0) (2026-04-21)


### Features

* **cli:** cache wizard commit subject from branch suggestions ([#117](https://github.com/kunchenguid/no-mistakes/issues/117)) ([134fadb](https://github.com/kunchenguid/no-mistakes/commit/134fadb3273e11835bcb43b6d8c8866a14d3575a))
* **wizard:** show setup progress in terminal title ([#116](https://github.com/kunchenguid/no-mistakes/issues/116)) ([4d42a7a](https://github.com/kunchenguid/no-mistakes/commit/4d42a7adabd63c888c1e970829be766897306530))


### Bug Fixes

* **daemon:** harden orphaned agent server recovery ([#121](https://github.com/kunchenguid/no-mistakes/issues/121)) ([7600bb7](https://github.com/kunchenguid/no-mistakes/commit/7600bb7de56772dd67c6e023c5deec2608d0572b))
* **daemon:** tighten detached fallback around managed service stop failures ([#115](https://github.com/kunchenguid/no-mistakes/issues/115)) ([957065a](https://github.com/kunchenguid/no-mistakes/commit/957065a3637cc0b971eadb8d651ecb95feebc76e))
* **pipeline:** harden CI autofix rerun retry guards ([#123](https://github.com/kunchenguid/no-mistakes/issues/123)) ([ac8cb83](https://github.com/kunchenguid/no-mistakes/commit/ac8cb8360e0dafb90ac1e0c9b13db041cc65d35f))
* **release:** run publish jobs after skipped validation ([#113](https://github.com/kunchenguid/no-mistakes/issues/113)) ([2fa5d24](https://github.com/kunchenguid/no-mistakes/commit/2fa5d24850aa4bd1fd3b24693554257b32a9d2e5))
* **wizard:** wait for daemon run before attach handoff ([#119](https://github.com/kunchenguid/no-mistakes/issues/119)) ([ca47098](https://github.com/kunchenguid/no-mistakes/commit/ca47098b34040a18765d95922eb3158052792f28))

## [1.8.1](https://github.com/kunchenguid/no-mistakes/compare/v1.8.0...v1.8.1) (2026-04-21)


### Bug Fixes

* **tui:** backfill rerun pipeline steps without stale placeholders ([#111](https://github.com/kunchenguid/no-mistakes/issues/111)) ([ae4ae8b](https://github.com/kunchenguid/no-mistakes/commit/ae4ae8b91cbc7c830a7ce05b5728887d56f6dc67))

## [1.8.0](https://github.com/kunchenguid/no-mistakes/compare/v1.7.0...v1.8.0) (2026-04-20)


### Features

* **cli:** keep the setup wizard visible for interactive --yes runs ([#109](https://github.com/kunchenguid/no-mistakes/issues/109)) ([dffa857](https://github.com/kunchenguid/no-mistakes/commit/dffa857b721db761c0505ca43af785f4d453f137))


### Bug Fixes

* **daemon:** update panic error status after telemetry ([#106](https://github.com/kunchenguid/no-mistakes/issues/106)) ([c188e12](https://github.com/kunchenguid/no-mistakes/commit/c188e12cf47f144de9ad02a083cccceec315a063))
* **wizard:** render branch step input and status inline ([#108](https://github.com/kunchenguid/no-mistakes/issues/108)) ([d945e2d](https://github.com/kunchenguid/no-mistakes/commit/d945e2dbd6130504a9f3e4861ca8f37ef1c0eda2))

## [1.7.0](https://github.com/kunchenguid/no-mistakes/compare/v1.6.0...v1.7.0) (2026-04-20)


### Features

* **cli:** add daemon restart command ([#102](https://github.com/kunchenguid/no-mistakes/issues/102)) ([795a42d](https://github.com/kunchenguid/no-mistakes/commit/795a42da12eb72bd1952beda902f77f79a6cc2ea))
* **cli:** add non-interactive setup wizard with --yes ([#105](https://github.com/kunchenguid/no-mistakes/issues/105)) ([99d2715](https://github.com/kunchenguid/no-mistakes/commit/99d27152f064a78864dc0fe08392d0d41f545d3f))
* **docs:** add custom hero CSS and update docs ([8d5e020](https://github.com/kunchenguid/no-mistakes/commit/8d5e0209195b67b366cf192f8330cf8dc21bfec1))
* **pipeline:** add testing details to PR summaries ([#104](https://github.com/kunchenguid/no-mistakes/issues/104)) ([7adb743](https://github.com/kunchenguid/no-mistakes/commit/7adb743b14033508a4f6f7aa42631d9a0ae6e914))

## [1.6.0](https://github.com/kunchenguid/no-mistakes/compare/v1.5.0...v1.6.0) (2026-04-18)


### Features

* **telemetry:** add CLI and pipeline telemetry tracking ([#98](https://github.com/kunchenguid/no-mistakes/issues/98)) ([234a900](https://github.com/kunchenguid/no-mistakes/commit/234a9005da13b7eede50ead93393776f55eb977f))
* **telemetry:** track wizard and tui pageviews ([#100](https://github.com/kunchenguid/no-mistakes/issues/100)) ([6056357](https://github.com/kunchenguid/no-mistakes/commit/605635782508f6ce5764c09d6c0d6f3eb215f936))

## [1.5.0](https://github.com/kunchenguid/no-mistakes/compare/v1.4.0...v1.5.0) (2026-04-17)


### Features

* **pipeline:** track round history across fix cycles ([#94](https://github.com/kunchenguid/no-mistakes/issues/94)) ([9596176](https://github.com/kunchenguid/no-mistakes/commit/9596176c4943665e9ecf4abf3b605b3c198c1634))
* **wizard:** add branch-aware setup flow for run startup ([#96](https://github.com/kunchenguid/no-mistakes/issues/96)) ([da3daf4](https://github.com/kunchenguid/no-mistakes/commit/da3daf4a0dd964417986e507ee3fe6dd6d54b6fd))


### Bug Fixes

* **agent:** harden rovodev SSE parsing and server health checks ([#93](https://github.com/kunchenguid/no-mistakes/issues/93)) ([e7e377f](https://github.com/kunchenguid/no-mistakes/commit/e7e377ff347c63c801b4143e6d28af3955f68bf9))
* **pipeline:** generate MP4 demos and gate review fixes ([#91](https://github.com/kunchenguid/no-mistakes/issues/91)) ([87b6801](https://github.com/kunchenguid/no-mistakes/commit/87b68013ba7ad050574804249be7d6602e399ddb))

## [1.4.0](https://github.com/kunchenguid/no-mistakes/compare/v1.3.0...v1.4.0) (2026-04-16)


### Features

* **bitbucket:** add Bitbucket Cloud PR and CI support ([#85](https://github.com/kunchenguid/no-mistakes/issues/85)) ([800f0e9](https://github.com/kunchenguid/no-mistakes/commit/800f0e9a4db73deb46cf6bca3aa2858c59552ee1))


### Bug Fixes

* **pipeline:** tighten PR title scope selection ([#87](https://github.com/kunchenguid/no-mistakes/issues/87)) ([08bb185](https://github.com/kunchenguid/no-mistakes/commit/08bb1857b6d1843e4625452c390cc70a6dc7bbd4))

## [1.3.0](https://github.com/kunchenguid/no-mistakes/compare/v1.2.1...v1.3.0) (2026-04-16)


### Features

* **daemon:** add managed daemon service startup with fallback ([#78](https://github.com/kunchenguid/no-mistakes/issues/78)) ([c52463f](https://github.com/kunchenguid/no-mistakes/commit/c52463fe4e053fb898653e585d2c86bec38f9c5f))
* **demo:** add scripted demo pipeline workflow ([#83](https://github.com/kunchenguid/no-mistakes/issues/83)) ([af25153](https://github.com/kunchenguid/no-mistakes/commit/af25153229303fa782c480b8ba016ac8b6d1d6b1))


### Bug Fixes

* **daemon:** ensure service manager is bypassed during go test ([#82](https://github.com/kunchenguid/no-mistakes/issues/82)) ([fef7527](https://github.com/kunchenguid/no-mistakes/commit/fef752776b93c8cc59f584eac22edf4a001396ce))
* **daemon:** scope managed service names by NM_HOME ([#84](https://github.com/kunchenguid/no-mistakes/issues/84)) ([65375dc](https://github.com/kunchenguid/no-mistakes/commit/65375dc3ff98f7ed61f17358df5cb19d1c555e37))
* **review:** add note to avoid running tests during review ([#81](https://github.com/kunchenguid/no-mistakes/issues/81)) ([594db31](https://github.com/kunchenguid/no-mistakes/commit/594db3189c45c6882a3a13fedc1de2ce7a6589a0))

## [1.2.1](https://github.com/kunchenguid/no-mistakes/compare/v1.2.0...v1.2.1) (2026-04-15)


### Bug Fixes

* **prompts:** ensure documentation prompts are updated ([#74](https://github.com/kunchenguid/no-mistakes/issues/74)) ([37f5e76](https://github.com/kunchenguid/no-mistakes/commit/37f5e76510c0776a999847564f842180b8e77c72))
* **update:** guard daemon restarts by executable path ([#76](https://github.com/kunchenguid/no-mistakes/issues/76)) ([8d2782d](https://github.com/kunchenguid/no-mistakes/commit/8d2782ddbef6ff5820b4a5e9ed06b2f0eb2773af))

## [1.2.0](https://github.com/kunchenguid/no-mistakes/compare/v1.1.1...v1.2.0) (2026-04-15)


### Features

* **tui:** add rerun action for completed pipeline runs ([#71](https://github.com/kunchenguid/no-mistakes/issues/71)) ([0759e07](https://github.com/kunchenguid/no-mistakes/commit/0759e077a03b71de8f79a87fe85858ce949ac0f9))
* **tui:** show cached update indicator in footer ([#72](https://github.com/kunchenguid/no-mistakes/issues/72)) ([09f0e0d](https://github.com/kunchenguid/no-mistakes/commit/09f0e0d8b2d0e1e82fe07b759f670b9e5f1f0d10))


### Bug Fixes

* **ci:** handle merge conflicts in babysit and harden mergeability checks ([#69](https://github.com/kunchenguid/no-mistakes/issues/69)) ([9e86144](https://github.com/kunchenguid/no-mistakes/commit/9e861448314cc4ccddd259e5acd1f1bd03ec73ba))
* **pipeline:** rename follow-up fix rounds to auto-fix ([#73](https://github.com/kunchenguid/no-mistakes/issues/73)) ([23e92a8](https://github.com/kunchenguid/no-mistakes/commit/23e92a826ea635c2614ab497ca729f500323b210))
* **tui:** use available height for stacked log tail ([#68](https://github.com/kunchenguid/no-mistakes/issues/68)) ([4a5a99a](https://github.com/kunchenguid/no-mistakes/commit/4a5a99ab9484eba0091c17294891138e8d89ff6a))
* updater self-update and install.sh for user-owned paths on macOS ([#66](https://github.com/kunchenguid/no-mistakes/issues/66)) ([119665e](https://github.com/kunchenguid/no-mistakes/commit/119665e8843ffb0360feea9ef74f59f803a5a34c))
* **update:** reset daemon after self-update and document failure handling ([#70](https://github.com/kunchenguid/no-mistakes/issues/70)) ([c1001d8](https://github.com/kunchenguid/no-mistakes/commit/c1001d8da5e12e5c51069b84752b4cf298617fde))

## [1.1.1](https://github.com/kunchenguid/no-mistakes/compare/v1.1.0...v1.1.1) (2026-04-15)


### Bug Fixes

* **config:** disable auto-fix review by default ([#63](https://github.com/kunchenguid/no-mistakes/issues/63)) ([c7a55df](https://github.com/kunchenguid/no-mistakes/commit/c7a55dfcb2ce6f334596f59721176d88d7eddd0f))

## [1.1.0](https://github.com/kunchenguid/no-mistakes/compare/v1.0.0...v1.1.0) (2026-04-14)


### Features

* add risk assessment, simplify icons, dedupe box rendering ([#7](https://github.com/kunchenguid/no-mistakes/issues/7)) ([cec663c](https://github.com/kunchenguid/no-mistakes/commit/cec663c27d2c1aff7500b313657ba93c51fb5698))
* Add Windows support for daemon IPC ([#4](https://github.com/kunchenguid/no-mistakes/issues/4)) ([53b06e6](https://github.com/kunchenguid/no-mistakes/commit/53b06e6e3b220f2fffb5268c18fc68bec7abdd16))
* **cli:** add styled output for interactive and non-interactive commands ([#17](https://github.com/kunchenguid/no-mistakes/issues/17)) ([06fb84b](https://github.com/kunchenguid/no-mistakes/commit/06fb84b8801384ded0754b9b522d916091798817))
* **config:** add auto agent detection and diagnostics ([#53](https://github.com/kunchenguid/no-mistakes/issues/53)) ([4d64ffe](https://github.com/kunchenguid/no-mistakes/commit/4d64ffec3a0ec701673c25aa0d343616e8dd9e9e))
* **db:** prefer active run for the current branch ([#21](https://github.com/kunchenguid/no-mistakes/issues/21)) ([940fd91](https://github.com/kunchenguid/no-mistakes/commit/940fd91d36ecae10d8904690cf7f644cd036fdec))
* **document:** add document pipeline step and tighten autofix review handling ([#35](https://github.com/kunchenguid/no-mistakes/issues/35)) ([61f5319](https://github.com/kunchenguid/no-mistakes/commit/61f53194a3e9b335847bef0cc6ebb1c9e0dd47b3))
* generate default global config on first daemon start ([#11](https://github.com/kunchenguid/no-mistakes/issues/11)) ([a00aedd](https://github.com/kunchenguid/no-mistakes/commit/a00aeddd4f02ecb76a3da144b6028333a13240d8))
* **pipeline:** add PR summary step and harden findings reporting ([#24](https://github.com/kunchenguid/no-mistakes/issues/24)) ([cc78cbf](https://github.com/kunchenguid/no-mistakes/commit/cc78cbfdc44ea0da1bdcfef0e69e0bbf5f29fc40))
* **pipeline:** persist and sanitize dismissed findings across review cycles ([#27](https://github.com/kunchenguid/no-mistakes/issues/27)) ([92de430](https://github.com/kunchenguid/no-mistakes/commit/92de4302ee0fed0a4ca8ea91f95e72bc5e0f15bf))
* **pipeline:** skip remaining steps on empty diff ([#50](https://github.com/kunchenguid/no-mistakes/issues/50)) ([4d74bc2](https://github.com/kunchenguid/no-mistakes/commit/4d74bc22ff8cf85f18806b10c4943c74d7cf511c))
* **pr-url:** add PR URL handling to events and UI ([#20](https://github.com/kunchenguid/no-mistakes/issues/20)) ([bded084](https://github.com/kunchenguid/no-mistakes/commit/bded084dd3047fd86ee13816b335e01e5553755b))
* **prsummary:** improve generated PR description output ([#57](https://github.com/kunchenguid/no-mistakes/issues/57)) ([bb4f0bc](https://github.com/kunchenguid/no-mistakes/commit/bb4f0bc3e285163e428d3bacf94aa7ac4a7be1f2))
* **rebase:** add scoped auto-fix support for rebase conflicts ([#30](https://github.com/kunchenguid/no-mistakes/issues/30)) ([13d379b](https://github.com/kunchenguid/no-mistakes/commit/13d379b30cf8444c7d85b40474a502e50fa5280c))
* **rebase:** agent-assisted conflict resolution and execution-only step duration ([#16](https://github.com/kunchenguid/no-mistakes/issues/16)) ([3ef3d01](https://github.com/kunchenguid/no-mistakes/commit/3ef3d01c0051ecabdff0dea3b75f9fc7514ded75))
* **review:** add configurable auto-fix retries and manual babysit fixes ([#15](https://github.com/kunchenguid/no-mistakes/issues/15)) ([3d71a89](https://github.com/kunchenguid/no-mistakes/commit/3d71a89d5fe5926029e43383708b1072bcf6efd2))
* **tui:** add open PR action ([#29](https://github.com/kunchenguid/no-mistakes/issues/29)) ([ae581c8](https://github.com/kunchenguid/no-mistakes/commit/ae581c8ee60cea36d2bd9fde519c94234bc03cf6))
* **tui:** manage terminal titles across run lifecycle ([#23](https://github.com/kunchenguid/no-mistakes/issues/23)) ([c5957d5](https://github.com/kunchenguid/no-mistakes/commit/c5957d566fceff074170c83e9a8c76e28b0a8364))


### Bug Fixes

* Add configurable grace period before exiting on empty CI checks ([#8](https://github.com/kunchenguid/no-mistakes/issues/8)) ([7908189](https://github.com/kunchenguid/no-mistakes/commit/7908189ebcc69e48409958662665326617f98074))
* **agent:** improve log rendering and add separators ([61e44c0](https://github.com/kunchenguid/no-mistakes/commit/61e44c0afc8d809acd4e03f470f86a520f6dabaa))
* **agent:** retry when Claude returns no structured output ([#47](https://github.com/kunchenguid/no-mistakes/issues/47)) ([6a5784c](https://github.com/kunchenguid/no-mistakes/commit/6a5784c266696ec2ec9cd92fe1644db718090ca8))
* **babysit:** remove PR comment handling, keep CI-only monitoring ([#12](https://github.com/kunchenguid/no-mistakes/issues/12)) ([bc10e51](https://github.com/kunchenguid/no-mistakes/commit/bc10e51b15188fbd407088ace370a9a4c063c00c))
* **banner:** add banner line ([#38](https://github.com/kunchenguid/no-mistakes/issues/38)) ([a9740ad](https://github.com/kunchenguid/no-mistakes/commit/a9740adf20fbcbd02c5b0dbc1af2075c00759d8a))
* **ci:** improve auto-fix no-change handling and reporting ([#55](https://github.com/kunchenguid/no-mistakes/issues/55)) ([174dbeb](https://github.com/kunchenguid/no-mistakes/commit/174dbebbbfe934cf84c9ce03125a812574489222))
* **config:** enable rebase auto-fix by default ([#48](https://github.com/kunchenguid/no-mistakes/issues/48)) ([55a12c5](https://github.com/kunchenguid/no-mistakes/commit/55a12c545561dfe57aec628cfd1b6bae49e91e19))
* **document:** validate findings payloads and document auto-fix flow ([#43](https://github.com/kunchenguid/no-mistakes/issues/43)) ([0ab485e](https://github.com/kunchenguid/no-mistakes/commit/0ab485e8b4cd6aa65ca21e995699c3f681373d55))
* gate human review and make push banner ASCII-safe ([#31](https://github.com/kunchenguid/no-mistakes/issues/31)) ([64f0665](https://github.com/kunchenguid/no-mistakes/commit/64f066551a9629c44fa5f5c7c4610353aebd3296))
* **ipc:** add daemon request logging without health noise ([#58](https://github.com/kunchenguid/no-mistakes/issues/58)) ([a5d8c22](https://github.com/kunchenguid/no-mistakes/commit/a5d8c229bb0e6a1ffd483b234e95594c72d3e8af))
* **opencode:** correct text streaming for review snapshots ([#14](https://github.com/kunchenguid/no-mistakes/issues/14)) ([e9a22ed](https://github.com/kunchenguid/no-mistakes/commit/e9a22ed0779c6cae4d85179ea7da8782cc2dfb87))
* **pipeline:** add discrete log handling and tests ([#60](https://github.com/kunchenguid/no-mistakes/issues/60)) ([75cc374](https://github.com/kunchenguid/no-mistakes/commit/75cc374b9a4a1bed46fbfcee8ace027d716b543b))
* **pipeline:** honor step env for CI and PR commands ([#59](https://github.com/kunchenguid/no-mistakes/issues/59)) ([0d5e739](https://github.com/kunchenguid/no-mistakes/commit/0d5e73923e3f09ab049796b92792a87ccf5ff38f))
* **pipeline:** improve risk handling in PR summary and review ([#45](https://github.com/kunchenguid/no-mistakes/issues/45)) ([31b9079](https://github.com/kunchenguid/no-mistakes/commit/31b9079e6c7668e8c74ef53471f6350aadb52fac))
* **pipeline:** restore findings compatibility and harden review intent ([#51](https://github.com/kunchenguid/no-mistakes/issues/51)) ([1b93f60](https://github.com/kunchenguid/no-mistakes/commit/1b93f6016577f66982f82c03923685e71bb629d1))
* **pr-title:** enforce conventional commit format on PR titles ([#10](https://github.com/kunchenguid/no-mistakes/issues/10)) ([5d4c357](https://github.com/kunchenguid/no-mistakes/commit/5d4c357cf6c08e1ccbe0c91ad708e69b4a0dc937))
* **pr:** improve risk summary output and remove hardcoded repo link ([#33](https://github.com/kunchenguid/no-mistakes/issues/33)) ([b266e9a](https://github.com/kunchenguid/no-mistakes/commit/b266e9a631ef9d0746620ee926e18466e5ac1230))
* **prsummary:** link pipeline summary tagline ([#49](https://github.com/kunchenguid/no-mistakes/issues/49)) ([d8c80db](https://github.com/kunchenguid/no-mistakes/commit/d8c80dbeb9598bfa9a53d9a7e6ad6132e5d12756))
* **prsummary:** preserve risk visibility in PR summaries ([#28](https://github.com/kunchenguid/no-mistakes/issues/28)) ([b080cd8](https://github.com/kunchenguid/no-mistakes/commit/b080cd8fa39d35281d1b1e2e0a6be9f76706e722))
* **pr:** unwrap nested PR body JSON and improve summary handling ([#46](https://github.com/kunchenguid/no-mistakes/issues/46)) ([cd3cdc3](https://github.com/kunchenguid/no-mistakes/commit/cd3cdc3831a79dc69907fc51d1c4d3110e21f120))
* **rebase:** harden force-push handling ([#54](https://github.com/kunchenguid/no-mistakes/issues/54)) ([7f18853](https://github.com/kunchenguid/no-mistakes/commit/7f18853913aa0336413327e0163593e46c71abd4))
* remove doc guard ([#52](https://github.com/kunchenguid/no-mistakes/issues/52)) ([bdb902f](https://github.com/kunchenguid/no-mistakes/commit/bdb902f0a4a1e453157c936ecfa65355e3d938e7))
* **review:** harden autofix prompt guards and findings sanitization ([#41](https://github.com/kunchenguid/no-mistakes/issues/41)) ([31eacf6](https://github.com/kunchenguid/no-mistakes/commit/31eacf6b263f30aad27799594db803aca81fca51))
* **review:** remove commit subjects from the review prompt ([#56](https://github.com/kunchenguid/no-mistakes/issues/56)) ([f6d729e](https://github.com/kunchenguid/no-mistakes/commit/f6d729ed8038d88c156e0e024a155cd6dc907b7c))
* safe guard reverting ([#39](https://github.com/kunchenguid/no-mistakes/issues/39)) ([ccdf75e](https://github.com/kunchenguid/no-mistakes/commit/ccdf75ed9e5b190b76b238e7d5058adbbb50e14d))
* **test-step:** add empty findings handling in test step ([#9](https://github.com/kunchenguid/no-mistakes/issues/9)) ([2701d6f](https://github.com/kunchenguid/no-mistakes/commit/2701d6ff5240aa767cb5e25465ddf9f4d437823f))
* **tui:** clamp babysit pipeline height in stacked layout ([#19](https://github.com/kunchenguid/no-mistakes/issues/19)) ([a756d76](https://github.com/kunchenguid/no-mistakes/commit/a756d76d8b39f737ef9b049eda08445f55f69d17))
* **tui:** correct timer handling for fixing status ([#44](https://github.com/kunchenguid/no-mistakes/issues/44)) ([a59ded4](https://github.com/kunchenguid/no-mistakes/commit/a59ded45903671e9f806f12669e5cdc4ea2138ce))
* **tui:** preserve accumulated timer when step auto-fixes ([#61](https://github.com/kunchenguid/no-mistakes/issues/61)) ([b709608](https://github.com/kunchenguid/no-mistakes/commit/b709608097795fc19d9d737d701a1f7ab5f8e9d6))
* **tui:** preserve and flush review log output ([#18](https://github.com/kunchenguid/no-mistakes/issues/18)) ([1bc6004](https://github.com/kunchenguid/no-mistakes/commit/1bc6004e31cba391e26b8c817bf185e866148b0a))
* **tui:** preserve help and action bar space with responsive logs ([#13](https://github.com/kunchenguid/no-mistakes/issues/13)) ([3e9fb8c](https://github.com/kunchenguid/no-mistakes/commit/3e9fb8cc8a352bea3aa7177dadc6d3a0fa27fde2))
* **tui:** show findings navigation hint for multiple findings ([#62](https://github.com/kunchenguid/no-mistakes/issues/62)) ([8c65d6b](https://github.com/kunchenguid/no-mistakes/commit/8c65d6b928653550e2f06f38f8a62c312031072b))
* **tui:** stabilize babysit panel layout and status context ([#22](https://github.com/kunchenguid/no-mistakes/issues/22)) ([7a1df93](https://github.com/kunchenguid/no-mistakes/commit/7a1df93dcb25cc6aefa46186e3f57a1c54429533))
* **tui:** update terminal title formatting and tests ([#25](https://github.com/kunchenguid/no-mistakes/issues/25)) ([137a27b](https://github.com/kunchenguid/no-mistakes/commit/137a27ba7c447b39d10e9fa61a7b6fa8800f1395))

## 1.0.0 (2026-04-11)


### Features

* e2e implementation ([e7e6bef](https://github.com/kunchenguid/no-mistakes/commit/e7e6bef67f5e5ffa39bcfdb76998cf409e06fe90))
* initial implementation ([3ff337b](https://github.com/kunchenguid/no-mistakes/commit/3ff337b76664dc7fc090eabff8fec937dbfd0d3b))
* **makefile:** add daemon start/stop to install ([818ad06](https://github.com/kunchenguid/no-mistakes/commit/818ad062ae50f305055903ffbd36bb75fbc52df8))
* **pipeline:** add cancel run support ([ea5056f](https://github.com/kunchenguid/no-mistakes/commit/ea5056f261cb8f1765307a7f88dcf810023ced9e))
* **pipeline:** add rebase step and fetch default branch ([a599581](https://github.com/kunchenguid/no-mistakes/commit/a599581788dcdb8f08bd52076a213f3a7594f5a7))
* **pipeline:** use branch base SHA for diffs ([51473e9](https://github.com/kunchenguid/no-mistakes/commit/51473e9dab77eb34ce1d9464f6bedf5646e85fd7))
* **tui:** add responsive layout for wide terminals ([dd0120c](https://github.com/kunchenguid/no-mistakes/commit/dd0120c6fbad3aba38a705c73c36b7e90469645d))
* **tui:** improve pipeline header and help overlay layout ([3643ab0](https://github.com/kunchenguid/no-mistakes/commit/3643ab04fcbdd7bf30da5ae116f07630668434f6))


### Bug Fixes

* Fix push step and harden pipeline commit handling ([#3](https://github.com/kunchenguid/no-mistakes/issues/3)) ([97330c4](https://github.com/kunchenguid/no-mistakes/commit/97330c4678da1c7ca02df40b81713abb6153b190))
