# Changelog

## [1.40.2](https://github.com/kunchenguid/no-mistakes/compare/v1.40.1...v1.40.2) (2026-07-21)


### Bug Fixes

* handle oversized Claude repair prompts ([#526](https://github.com/kunchenguid/no-mistakes/issues/526)) ([857885f](https://github.com/kunchenguid/no-mistakes/commit/857885f7bcbbaf862eed3fe67acae5abef52ec7c))
* **scm/github:** target known PR explicitly from bare gate repo ([#522](https://github.com/kunchenguid/no-mistakes/issues/522)) ([4ba40ae](https://github.com/kunchenguid/no-mistakes/commit/4ba40aee93e80f7e9ee82f510c12eb5d9e26f3ea))
* **scm:** resolve SSH host aliases for provider routing ([#520](https://github.com/kunchenguid/no-mistakes/issues/520)) ([18456e4](https://github.com/kunchenguid/no-mistakes/commit/18456e4ef6b59a649ed6c537ec670b397225b92c))

## [1.40.1](https://github.com/kunchenguid/no-mistakes/compare/v1.40.0...v1.40.1) (2026-07-20)


### Bug Fixes

* **cli:** keep AXI output readable with control bytes ([#523](https://github.com/kunchenguid/no-mistakes/issues/523)) ([37864fe](https://github.com/kunchenguid/no-mistakes/commit/37864fe1053d9065b64522b515745793e7412f95))
* **scm/azuredevops:** pass PR description via temp file ([#501](https://github.com/kunchenguid/no-mistakes/issues/501)) ([#510](https://github.com/kunchenguid/no-mistakes/issues/510)) ([2d10688](https://github.com/kunchenguid/no-mistakes/commit/2d10688c1753de85358ce6a74b72d90e696c4f05))

## [1.40.0](https://github.com/kunchenguid/no-mistakes/compare/v1.39.0...v1.40.0) (2026-07-18)


### Features

* **agent:** add Cursor as a first-class ACP alias ([#422](https://github.com/kunchenguid/no-mistakes/issues/422)) ([8abc1ed](https://github.com/kunchenguid/no-mistakes/commit/8abc1ed63b5cdef5aca31d4f17c7482aa4275750))
* **branchsync:** reconcile equivalent diverged sync heads ([#504](https://github.com/kunchenguid/no-mistakes/issues/504)) ([0a2c82f](https://github.com/kunchenguid/no-mistakes/commit/0a2c82f993b9467c5ab84992313dfd13b66830af))
* **config:** configure pipeline fix commit messages ([#511](https://github.com/kunchenguid/no-mistakes/issues/511)) ([be6921e](https://github.com/kunchenguid/no-mistakes/commit/be6921ef4f739b7e068ec3a315617f3df328f6b6))


### Bug Fixes

* **cli:** reattach active AXI runs by submitted head ([#514](https://github.com/kunchenguid/no-mistakes/issues/514)) ([26200ed](https://github.com/kunchenguid/no-mistakes/commit/26200ed06a7ae460396937e49410974f2d2ac81f))

## [1.39.0](https://github.com/kunchenguid/no-mistakes/compare/v1.38.1...v1.39.0) (2026-07-16)


### Features

* **branchsync:** recover custody for terminal pipeline-owned runs ([#502](https://github.com/kunchenguid/no-mistakes/issues/502)) ([0bca236](https://github.com/kunchenguid/no-mistakes/commit/0bca23626016105c0c6f56a702b5b7ec3a86f42a))

## [1.38.1](https://github.com/kunchenguid/no-mistakes/compare/v1.38.0...v1.38.1) (2026-07-16)


### Bug Fixes

* **pipeline:** use standard git credential env for steps ([#499](https://github.com/kunchenguid/no-mistakes/issues/499)) ([a87d451](https://github.com/kunchenguid/no-mistakes/commit/a87d4516381103722863ebbd29e3fc0aed61e591))

## [1.38.0](https://github.com/kunchenguid/no-mistakes/compare/v1.37.1...v1.38.0) (2026-07-16)


### Features

* **branchsync:** add guarded local branch synchronization ([#496](https://github.com/kunchenguid/no-mistakes/issues/496)) ([e9b4c14](https://github.com/kunchenguid/no-mistakes/commit/e9b4c14dfd6b444049e1d594b26e40124c4973a3))

## [1.37.1](https://github.com/kunchenguid/no-mistakes/compare/v1.37.0...v1.37.1) (2026-07-16)


### Bug Fixes

* **pipeline:** defer run-owned delivery findings ([#494](https://github.com/kunchenguid/no-mistakes/issues/494)) ([fc0845a](https://github.com/kunchenguid/no-mistakes/commit/fc0845a231628857a628b0f8818cc2a199c48eab))
* **pipeline:** reconcile parked CI gates ([#486](https://github.com/kunchenguid/no-mistakes/issues/486)) ([2bbbc14](https://github.com/kunchenguid/no-mistakes/commit/2bbbc143bd4520056e97957883a02615657b2a62))

## [1.37.0](https://github.com/kunchenguid/no-mistakes/compare/v1.36.0...v1.37.0) (2026-07-13)


### Features

* **agent:** record session-fidelity telemetry ([#457](https://github.com/kunchenguid/no-mistakes/issues/457)) ([8ec98db](https://github.com/kunchenguid/no-mistakes/commit/8ec98db2a39a054a90071dfd0c8175785c6343a9))
* **agent:** suppress project settings for gate agents ([#463](https://github.com/kunchenguid/no-mistakes/issues/463)) ([2d02c90](https://github.com/kunchenguid/no-mistakes/commit/2d02c90cc964c280a6b849380dab26d3224b4549))


### Bug Fixes

* **pipeline:** prevent clobbered HEAD from shipping unreviewed fixes ([#462](https://github.com/kunchenguid/no-mistakes/issues/462)) ([a7e71ea](https://github.com/kunchenguid/no-mistakes/commit/a7e71ea92f83ea4a597580dd4c30099d084f79de))

## [1.36.0](https://github.com/kunchenguid/no-mistakes/compare/v1.35.0...v1.36.0) (2026-07-12)


### Features

* Developer ID sign macOS release artifacts ([#455](https://github.com/kunchenguid/no-mistakes/issues/455)) ([776088d](https://github.com/kunchenguid/no-mistakes/commit/776088d179d06ebce78a932ab49373405fb35c0f))


### Bug Fixes

* **git:** resolve main repo root inside submodules ([#454](https://github.com/kunchenguid/no-mistakes/issues/454)) ([fc22899](https://github.com/kunchenguid/no-mistakes/commit/fc228993bd7c6ef873de01e07a57c69743dbdc5a))
* **pipeline:** enforce explicit intent during review ([#448](https://github.com/kunchenguid/no-mistakes/issues/448)) ([5b0cb40](https://github.com/kunchenguid/no-mistakes/commit/5b0cb40cccd7ee95a03c7199965dfd5c419bed08))
* **pipeline:** focus review fixer verification ([#453](https://github.com/kunchenguid/no-mistakes/issues/453)) ([bb0b6bd](https://github.com/kunchenguid/no-mistakes/commit/bb0b6bd6d1c6af9f74ca5c10c03866f85837fbc2))

## [1.35.0](https://github.com/kunchenguid/no-mistakes/compare/v1.34.2...v1.35.0) (2026-07-11)


### Features

* **pipeline:** reuse review sessions and streamline telemetry ([#444](https://github.com/kunchenguid/no-mistakes/issues/444)) ([48bd29f](https://github.com/kunchenguid/no-mistakes/commit/48bd29f6c04eef9876d4e85bf97af465de023df4))

## [1.34.2](https://github.com/kunchenguid/no-mistakes/compare/v1.34.1...v1.34.2) (2026-07-10)


### Bug Fixes

* **daemon:** require explicit daemon run argv, fail fast on dead socket ([#416](https://github.com/kunchenguid/no-mistakes/issues/416)) ([b876724](https://github.com/kunchenguid/no-mistakes/commit/b87672422163865f0e2dfcc3f1432887ccea2124))
* **git:** resolve gate hook paths robustly ([#383](https://github.com/kunchenguid/no-mistakes/issues/383)) ([df2db1e](https://github.com/kunchenguid/no-mistakes/commit/df2db1efd4d4d508679064481650195fb4b0b032))

## [1.34.1](https://github.com/kunchenguid/no-mistakes/compare/v1.34.0...v1.34.1) (2026-07-10)


### Bug Fixes

* **cli:** guard destructive daemon lifecycle commands against active runs ([#415](https://github.com/kunchenguid/no-mistakes/issues/415)) ([493fc69](https://github.com/kunchenguid/no-mistakes/commit/493fc69d62b3d6b841080233230ce90c5fcf7b6e))
* **config:** fail gate validation when no pipeline agent is runnable ([#437](https://github.com/kunchenguid/no-mistakes/issues/437)) ([3752c1a](https://github.com/kunchenguid/no-mistakes/commit/3752c1a0fb7b76ff40f83143eea799fbd6e7d5b0))
* **daemon:** bound IPC connect attempts with a configurable timeout ([#403](https://github.com/kunchenguid/no-mistakes/issues/403)) ([c663aee](https://github.com/kunchenguid/no-mistakes/commit/c663aeee95d34b07351c3a0fbd105acbfcf5ac3a))
* **update:** make --version a side-effect-free read-only probe ([#423](https://github.com/kunchenguid/no-mistakes/issues/423)) ([65e37bc](https://github.com/kunchenguid/no-mistakes/commit/65e37bcce93507fea3680abd28bcb9f0ace1b077))
* **winproc:** suppress console window flashing for child processes on windows ([#418](https://github.com/kunchenguid/no-mistakes/issues/418)) ([7625211](https://github.com/kunchenguid/no-mistakes/commit/7625211125c399c9665ff7e7edc1dd8aa17af075))

## [1.34.0](https://github.com/kunchenguid/no-mistakes/compare/v1.33.0...v1.34.0) (2026-07-07)


### Features

* **agent:** support ordered fallback agent lists ([#379](https://github.com/kunchenguid/no-mistakes/issues/379)) ([59278f1](https://github.com/kunchenguid/no-mistakes/commit/59278f156c670e568ca5b75d507746dd4ab92088))
* **cli:** surface AXI step activity and auto-fix diagnostics ([#413](https://github.com/kunchenguid/no-mistakes/issues/413)) ([6fcf9d5](https://github.com/kunchenguid/no-mistakes/commit/6fcf9d59df29facbcde28c5a4981187e9e2b5a90))
* **scm:** add GitHub Enterprise Server provider detection and host-prefixed slug ([#377](https://github.com/kunchenguid/no-mistakes/issues/377)) ([d4f9274](https://github.com/kunchenguid/no-mistakes/commit/d4f927462ad942ed9fd50bf065f14bd66e0c5e92))


### Bug Fixes

* **daemon:** enforce single-daemon ownership per NM_HOME ([#411](https://github.com/kunchenguid/no-mistakes/issues/411)) ([bbdf1f0](https://github.com/kunchenguid/no-mistakes/commit/bbdf1f0be8edd5c2917cb6bcc8cc6441d1e6a78f))
* **git:** name bare gate repos explicitly with --git-dir ([#384](https://github.com/kunchenguid/no-mistakes/issues/384)) ([7bd8384](https://github.com/kunchenguid/no-mistakes/commit/7bd83841c930d4de4d902a1f3af7bea6d7386e6c))
* **git:** write worktree identity per-worktree to avoid shared config.lock ([#385](https://github.com/kunchenguid/no-mistakes/issues/385)) ([95b4482](https://github.com/kunchenguid/no-mistakes/commit/95b4482d2aaae1235b105ba948a839a8b74a291b))

## [1.33.0](https://github.com/kunchenguid/no-mistakes/compare/v1.32.2...v1.33.0) (2026-07-03)


### Features

* **scm:** add Azure DevOps provider ([#369](https://github.com/kunchenguid/no-mistakes/issues/369)) ([78c7e60](https://github.com/kunchenguid/no-mistakes/commit/78c7e606ce598491d50e72bf532045f4684ca8b7))


### Bug Fixes

* **agent:** surface opencode StructuredOutputError instead of text-parsing prose ([#375](https://github.com/kunchenguid/no-mistakes/issues/375)) ([02009a8](https://github.com/kunchenguid/no-mistakes/commit/02009a8535761eb41f8dea07118e71f86c9f0644))
* **daemon:** detect stray daemons by resolved root before start ([#360](https://github.com/kunchenguid/no-mistakes/issues/360)) ([6c59484](https://github.com/kunchenguid/no-mistakes/commit/6c594845054c896a316bfc124489725624323d8b))
* **daemon:** forward proxy env vars into managed daemon service definitions ([#322](https://github.com/kunchenguid/no-mistakes/issues/322)) ([03f5157](https://github.com/kunchenguid/no-mistakes/commit/03f515777c16db8fab0a58ee9000da86d409e6f4))
* **gate:** resolve absolute bare repo dir in post-receive hook ([#269](https://github.com/kunchenguid/no-mistakes/issues/269)) ([#358](https://github.com/kunchenguid/no-mistakes/issues/358)) ([087fd27](https://github.com/kunchenguid/no-mistakes/commit/087fd279bb227e2d9f6112afb4c2d2a100f4fa8a))
* **git:** absolutize PWD in NonInteractiveEnv ([#381](https://github.com/kunchenguid/no-mistakes/issues/381)) ([a84593c](https://github.com/kunchenguid/no-mistakes/commit/a84593cd4fa622d9aef38788db9c20acb7da431c))
* **pipeline:** cap generated PR bodies safely ([#370](https://github.com/kunchenguid/no-mistakes/issues/370)) ([9059685](https://github.com/kunchenguid/no-mistakes/commit/9059685ad88e30554e5228fbb45ffd70f61caf00))

## [1.32.2](https://github.com/kunchenguid/no-mistakes/compare/v1.32.1...v1.32.2) (2026-06-28)


### Bug Fixes

* **agent:** reap agent process group on clean exit to prevent daemon OOM crash ([#357](https://github.com/kunchenguid/no-mistakes/issues/357)) ([bdd2e39](https://github.com/kunchenguid/no-mistakes/commit/bdd2e3932e6b49830c622ed00cf7b99635688fca))

## [1.32.1](https://github.com/kunchenguid/no-mistakes/compare/v1.32.0...v1.32.1) (2026-06-28)


### Bug Fixes

* **cli:** clarify stale CI monitor guidance ([#352](https://github.com/kunchenguid/no-mistakes/issues/352)) ([87b2abf](https://github.com/kunchenguid/no-mistakes/commit/87b2abf78888d8af738903415f5f4b58e61e2396))

## [1.32.0](https://github.com/kunchenguid/no-mistakes/compare/v1.31.2...v1.32.0) (2026-06-27)


### Features

* **scm:** detect self-hosted GitLab out of the box with glab v1.5x ([#346](https://github.com/kunchenguid/no-mistakes/issues/346)) ([42ae9c2](https://github.com/kunchenguid/no-mistakes/commit/42ae9c280e78299b7e161e2e83dfbe10bc1478a2))

## [1.31.2](https://github.com/kunchenguid/no-mistakes/compare/v1.31.1...v1.31.2) (2026-06-27)


### Bug Fixes

* **cli:** clarify AXI gate guidance ([#344](https://github.com/kunchenguid/no-mistakes/issues/344)) ([8ebc9eb](https://github.com/kunchenguid/no-mistakes/commit/8ebc9ebfa2f592da09d5c54f89b634e8a1ec8bf8))

## [1.31.1](https://github.com/kunchenguid/no-mistakes/compare/v1.31.0...v1.31.1) (2026-06-26)


### Bug Fixes

* **pipeline:** guard rebase and force-push safety ([#341](https://github.com/kunchenguid/no-mistakes/issues/341)) ([5efd0b2](https://github.com/kunchenguid/no-mistakes/commit/5efd0b2005dcf6cea17edaf4fe10d0e65b51fc0c))

## [1.31.0](https://github.com/kunchenguid/no-mistakes/compare/v1.30.2...v1.31.0) (2026-06-26)


### Features

* **agent:** add GitHub Copilot CLI agent backend ([#318](https://github.com/kunchenguid/no-mistakes/issues/318)) ([aedb3a1](https://github.com/kunchenguid/no-mistakes/commit/aedb3a1406e734a696ee1182af28e804e8314f07))
* **cli:** expose parked awaiting-agent status ([#329](https://github.com/kunchenguid/no-mistakes/issues/329)) ([57a62da](https://github.com/kunchenguid/no-mistakes/commit/57a62da91d7089f80d92fc6f4373a49f1fe73319))


### Bug Fixes

* keep CI monitors active and abortable by run id ([#316](https://github.com/kunchenguid/no-mistakes/issues/316)) ([0e07573](https://github.com/kunchenguid/no-mistakes/commit/0e075737b0d55c3e8479b07991a7fd221c1264d1))

## [1.30.2](https://github.com/kunchenguid/no-mistakes/compare/v1.30.1...v1.30.2) (2026-06-21)


### Bug Fixes

* **security:** load repo config from default branch, not pushed SHA (supply-chain RCE) ([#297](https://github.com/kunchenguid/no-mistakes/issues/297)) ([21ff425](https://github.com/kunchenguid/no-mistakes/commit/21ff425c7e8c2dd3dda6149cbd47d32b8c7b5da7))

## [1.30.1](https://github.com/kunchenguid/no-mistakes/compare/v1.30.0...v1.30.1) (2026-06-21)


### Bug Fixes

* **agent:** kill child process group on cancel (no orphan subprocesses) ([#300](https://github.com/kunchenguid/no-mistakes/issues/300)) ([19045f6](https://github.com/kunchenguid/no-mistakes/commit/19045f6e04251f9b746467dcc6ad372cb4658f42))

## [1.30.0](https://github.com/kunchenguid/no-mistakes/compare/v1.29.1...v1.30.0) (2026-06-21)


### Features

* **git:** support GitHub fork routing ([#306](https://github.com/kunchenguid/no-mistakes/issues/306)) ([7974313](https://github.com/kunchenguid/no-mistakes/commit/79743139e5930ea1f61cee5e2733b0341a2081eb))

## [1.29.1](https://github.com/kunchenguid/no-mistakes/compare/v1.29.0...v1.29.1) (2026-06-13)


### Bug Fixes

* **cli:** keep AXI runs branch-aware ([#288](https://github.com/kunchenguid/no-mistakes/issues/288)) ([94afd17](https://github.com/kunchenguid/no-mistakes/commit/94afd170f44855aa55a782a9e3cbf6a64195ab35))

## [1.29.0](https://github.com/kunchenguid/no-mistakes/compare/v1.28.1...v1.29.0) (2026-06-12)


### Features

* **cli:** install agent skill at user level ([#284](https://github.com/kunchenguid/no-mistakes/issues/284)) ([f6760aa](https://github.com/kunchenguid/no-mistakes/commit/f6760aa115aaa3ea3efdb4402383af429ad5aaba))

## [1.28.1](https://github.com/kunchenguid/no-mistakes/compare/v1.28.0...v1.28.1) (2026-06-11)


### Bug Fixes

* **skill:** mark installed skills internal ([#279](https://github.com/kunchenguid/no-mistakes/issues/279)) ([d7878f7](https://github.com/kunchenguid/no-mistakes/commit/d7878f7184d476cb3ac2cfbbb06e7abb019694a3))

## [1.28.0](https://github.com/kunchenguid/no-mistakes/compare/v1.27.3...v1.28.0) (2026-06-10)


### Features

* **cli:** report applied fixes in successful AXI output ([#274](https://github.com/kunchenguid/no-mistakes/issues/274)) ([8271fef](https://github.com/kunchenguid/no-mistakes/commit/8271fef7b9852acbb4f2a8b8cedcfb4a061b3892))

## [1.27.3](https://github.com/kunchenguid/no-mistakes/compare/v1.27.2...v1.27.3) (2026-06-10)


### Bug Fixes

* **cli:** clarify pipeline-owned finding fixes ([#272](https://github.com/kunchenguid/no-mistakes/issues/272)) ([e6d46fd](https://github.com/kunchenguid/no-mistakes/commit/e6d46fd6cb120afe1da946822be8186f9d13d51b))

## [1.27.2](https://github.com/kunchenguid/no-mistakes/compare/v1.27.1...v1.27.2) (2026-06-09)


### Bug Fixes

* **gate:** reattach init after repo directory rename ([#270](https://github.com/kunchenguid/no-mistakes/issues/270)) ([a2f4fc2](https://github.com/kunchenguid/no-mistakes/commit/a2f4fc2afaa6fdff03d0854024703da10b14b498))

## [1.27.1](https://github.com/kunchenguid/no-mistakes/compare/v1.27.0...v1.27.1) (2026-06-09)


### Bug Fixes

* **document,review:** correct spelling errors (unparseable -&gt; unparsable, funcitonal -&gt; functional) ([#262](https://github.com/kunchenguid/no-mistakes/issues/262)) ([de69d5e](https://github.com/kunchenguid/no-mistakes/commit/de69d5e9881865c5da878044b58804ae10c2db83))

## [1.27.0](https://github.com/kunchenguid/no-mistakes/compare/v1.26.1...v1.27.0) (2026-06-08)


### Features

* **skill:** support task-first no-mistakes invocation ([#259](https://github.com/kunchenguid/no-mistakes/issues/259)) ([831c2c1](https://github.com/kunchenguid/no-mistakes/commit/831c2c159e9578b0425183fc472134b3fe4770b8))

## [1.26.1](https://github.com/kunchenguid/no-mistakes/compare/v1.26.0...v1.26.1) (2026-06-08)


### Bug Fixes

* **scm:** make GitHub operations repo-aware ([#256](https://github.com/kunchenguid/no-mistakes/issues/256)) ([5964ad9](https://github.com/kunchenguid/no-mistakes/commit/5964ad9d521860f314fb1c992b8fa7870db59fc3))

## [1.26.0](https://github.com/kunchenguid/no-mistakes/compare/v1.25.1...v1.26.0) (2026-06-08)


### Features

* **cli:** emit AXI pageview telemetry ([#252](https://github.com/kunchenguid/no-mistakes/issues/252)) ([ba97def](https://github.com/kunchenguid/no-mistakes/commit/ba97def4daaa4389b864685400759b91bdd96d14))

## [1.25.1](https://github.com/kunchenguid/no-mistakes/compare/v1.25.0...v1.25.1) (2026-06-08)


### Bug Fixes

* **skill:** escalate ask-user findings to users ([#248](https://github.com/kunchenguid/no-mistakes/issues/248)) ([c015392](https://github.com/kunchenguid/no-mistakes/commit/c015392d7d0e4c59ca6f887c9547186105684da3))
* **skill:** improve intent guidance completeness ([#250](https://github.com/kunchenguid/no-mistakes/issues/250)) ([4cebf97](https://github.com/kunchenguid/no-mistakes/commit/4cebf97fdb570d023b2b536ad8c8832e1e557575))
* **skill:** install skills through symlinked bases ([#251](https://github.com/kunchenguid/no-mistakes/issues/251)) ([3d285e7](https://github.com/kunchenguid/no-mistakes/commit/3d285e792dbe8a9d639f8600766a897ecb3ad541))

## [1.25.0](https://github.com/kunchenguid/no-mistakes/compare/v1.24.0...v1.25.0) (2026-06-07)


### Features

* auto-resolve actionable findings in yolo mode ([#246](https://github.com/kunchenguid/no-mistakes/issues/246)) ([1c99c4c](https://github.com/kunchenguid/no-mistakes/commit/1c99c4ce97d591fc4e7300c699389973b78b1c4b))
* **tui:** show checks-passed CI monitoring state ([#245](https://github.com/kunchenguid/no-mistakes/issues/245)) ([4d741be](https://github.com/kunchenguid/no-mistakes/commit/4d741bedf07a9ea6171089150d699e7e83174e8b))


### Bug Fixes

* **cli:** stop axi drive after CI checks pass ([#247](https://github.com/kunchenguid/no-mistakes/issues/247)) ([badf49f](https://github.com/kunchenguid/no-mistakes/commit/badf49f690f896e41a2d71fe59218f049260167f))
* **gate:** make init idempotent ([#243](https://github.com/kunchenguid/no-mistakes/issues/243)) ([0e1a08b](https://github.com/kunchenguid/no-mistakes/commit/0e1a08be7077117864faa854f9cdf08a1590c3e6))

## [1.24.0](https://github.com/kunchenguid/no-mistakes/compare/v1.23.1...v1.24.0) (2026-06-07)


### Features

* **cli:** add AXI agent command surface ([#241](https://github.com/kunchenguid/no-mistakes/issues/241)) ([d3a54e0](https://github.com/kunchenguid/no-mistakes/commit/d3a54e0050ee279b1aba9315be0fc4698ee020ee))

## [1.23.1](https://github.com/kunchenguid/no-mistakes/compare/v1.23.0...v1.23.1) (2026-06-07)


### Bug Fixes

* **pipeline:** keep monitoring open PRs after CI passes ([#240](https://github.com/kunchenguid/no-mistakes/issues/240)) ([41a03e2](https://github.com/kunchenguid/no-mistakes/commit/41a03e2a635c8a10d3d4b1de546fad4670464501))
* **update:** warn before updating with active runs ([#238](https://github.com/kunchenguid/no-mistakes/issues/238)) ([15951e1](https://github.com/kunchenguid/no-mistakes/commit/15951e122babd12afe6c3dcaf96816eae162cac0))

## [1.23.0](https://github.com/kunchenguid/no-mistakes/compare/v1.22.3...v1.23.0) (2026-06-06)


### Features

* **pipeline:** store test evidence in repos ([#235](https://github.com/kunchenguid/no-mistakes/issues/235)) ([ec6d7dc](https://github.com/kunchenguid/no-mistakes/commit/ec6d7dc34d7ae09279d483f185c9d47bbf10c66a))

## [1.22.3](https://github.com/kunchenguid/no-mistakes/compare/v1.22.2...v1.22.3) (2026-06-05)


### Bug Fixes

* **pipeline:** include step error output in diagnostics ([#233](https://github.com/kunchenguid/no-mistakes/issues/233)) ([bc8bd5a](https://github.com/kunchenguid/no-mistakes/commit/bc8bd5ac62cc5934bbf1ef276c20b0b1725dc38f))

## [1.22.2](https://github.com/kunchenguid/no-mistakes/compare/v1.22.1...v1.22.2) (2026-06-04)


### Bug Fixes

* **agent:** disable interactive git prompts ([#231](https://github.com/kunchenguid/no-mistakes/issues/231)) ([348390d](https://github.com/kunchenguid/no-mistakes/commit/348390d4316b51799ecdd618a7326265e417cee0))

## [1.22.1](https://github.com/kunchenguid/no-mistakes/compare/v1.22.0...v1.22.1) (2026-05-29)


### Bug Fixes

* **agent:** steer pipeline agents away from system writes ([#229](https://github.com/kunchenguid/no-mistakes/issues/229)) ([9f88c13](https://github.com/kunchenguid/no-mistakes/commit/9f88c1339af4cd7647d3de0eb5ce30fd7a656f71))

## [1.22.0](https://github.com/kunchenguid/no-mistakes/compare/v1.21.6...v1.22.0) (2026-05-28)


### Features

* **tui:** add yolo auto-approval mode ([#227](https://github.com/kunchenguid/no-mistakes/issues/227)) ([570c9f9](https://github.com/kunchenguid/no-mistakes/commit/570c9f9d8694c04c5a19219f5ac5f14f9301be34))

## [1.21.6](https://github.com/kunchenguid/no-mistakes/compare/v1.21.5...v1.21.6) (2026-05-28)


### Bug Fixes

* **pipeline:** let document step fix documentation gaps directly ([#226](https://github.com/kunchenguid/no-mistakes/issues/226)) ([82974e9](https://github.com/kunchenguid/no-mistakes/commit/82974e9dd84fdb48a2864eaa994c12f176ce7382))
* **tui:** keep fix review row within width ([#224](https://github.com/kunchenguid/no-mistakes/issues/224)) ([abb1d9a](https://github.com/kunchenguid/no-mistakes/commit/abb1d9a285f998e3c4761d832688b112795ddbbc))

## [1.21.5](https://github.com/kunchenguid/no-mistakes/compare/v1.21.4...v1.21.5) (2026-05-27)


### Bug Fixes

* **pipeline:** embed text evidence in PR summaries ([#222](https://github.com/kunchenguid/no-mistakes/issues/222)) ([4eb3dd9](https://github.com/kunchenguid/no-mistakes/commit/4eb3dd9b1cc3bf0704c7981fc665852f0bba6f2a))
* **pipeline:** keep inline code prose in PR summaries ([#220](https://github.com/kunchenguid/no-mistakes/issues/220)) ([94a7361](https://github.com/kunchenguid/no-mistakes/commit/94a736185220f774fa2ea49921d54b11b3c0a89c))
* **pipeline:** narrate PR auto-fix summaries ([#223](https://github.com/kunchenguid/no-mistakes/issues/223)) ([902c0c5](https://github.com/kunchenguid/no-mistakes/commit/902c0c581cfbd01cc4f244f751c676a2bb64cf7e))

## [1.21.4](https://github.com/kunchenguid/no-mistakes/compare/v1.21.3...v1.21.4) (2026-05-27)


### Bug Fixes

* **agent:** extend managed server health timeout ([#218](https://github.com/kunchenguid/no-mistakes/issues/218)) ([403b142](https://github.com/kunchenguid/no-mistakes/commit/403b142362d4dbeb71dbac2c788febb1f4cb7233))

## [1.21.3](https://github.com/kunchenguid/no-mistakes/compare/v1.21.2...v1.21.3) (2026-05-23)


### Bug Fixes

* **shellenv:** retry degraded daemon environment fallback ([#216](https://github.com/kunchenguid/no-mistakes/issues/216)) ([14cd1dd](https://github.com/kunchenguid/no-mistakes/commit/14cd1dd3f0d64d9cbdb3d86c6f5318b650549ff6))

## [1.21.2](https://github.com/kunchenguid/no-mistakes/compare/v1.21.1...v1.21.2) (2026-05-23)


### Bug Fixes

* **pipeline:** preserve local test evidence in PR summaries ([#214](https://github.com/kunchenguid/no-mistakes/issues/214)) ([09147b2](https://github.com/kunchenguid/no-mistakes/commit/09147b23f53f7a07cb910e5e50dede0aec8dbc3c))

## [1.21.1](https://github.com/kunchenguid/no-mistakes/compare/v1.21.0...v1.21.1) (2026-05-21)


### Bug Fixes

* **pipeline:** ignore deleted files for intent matching ([#212](https://github.com/kunchenguid/no-mistakes/issues/212)) ([ca17d1e](https://github.com/kunchenguid/no-mistakes/commit/ca17d1ed2db2c58c066fb0c018ebcc9b7b253f2c))

## [1.21.0](https://github.com/kunchenguid/no-mistakes/compare/v1.20.1...v1.21.0) (2026-05-19)


### Features

* **intent:** support Pi transcript reading ([#210](https://github.com/kunchenguid/no-mistakes/issues/210)) ([f21f1f6](https://github.com/kunchenguid/no-mistakes/commit/f21f1f6984070017030649094d6b57053ea8a177))

## [1.20.1](https://github.com/kunchenguid/no-mistakes/compare/v1.20.0...v1.20.1) (2026-05-18)


### Bug Fixes

* **intent:** log accepted candidates only ([#209](https://github.com/kunchenguid/no-mistakes/issues/209)) ([c04a2a7](https://github.com/kunchenguid/no-mistakes/commit/c04a2a7ee806b192a99c1c8073358ff9938527ee))
* **pipeline:** extend intent extraction timeout ([#206](https://github.com/kunchenguid/no-mistakes/issues/206)) ([90a1829](https://github.com/kunchenguid/no-mistakes/commit/90a1829a68a61adef7bf18af87e283140181e0cb))
* **pipeline:** render PR testing summaries as prose ([#208](https://github.com/kunchenguid/no-mistakes/issues/208)) ([6e4d7e1](https://github.com/kunchenguid/no-mistakes/commit/6e4d7e1c5ead3b75cf2265b490afd06c8c05ef5d))

## [1.20.0](https://github.com/kunchenguid/no-mistakes/compare/v1.19.3...v1.20.0) (2026-05-16)


### Features

* **intent:** disambiguate ambiguous session matches ([#204](https://github.com/kunchenguid/no-mistakes/issues/204)) ([f784c80](https://github.com/kunchenguid/no-mistakes/commit/f784c8052239b8f9c9e3fe08abe349c223cc6ab8))

## [1.19.3](https://github.com/kunchenguid/no-mistakes/compare/v1.19.2...v1.19.3) (2026-05-16)


### Bug Fixes

* **update:** confirm daemon takeover before replacing running daemon ([#200](https://github.com/kunchenguid/no-mistakes/issues/200)) ([5fd0cf2](https://github.com/kunchenguid/no-mistakes/commit/5fd0cf254e9f9dc31b5d478514c03cdf313e833c))

## [1.19.2](https://github.com/kunchenguid/no-mistakes/compare/v1.19.1...v1.19.2) (2026-05-16)


### Bug Fixes

* **intent:** improve transcript session matching ([#201](https://github.com/kunchenguid/no-mistakes/issues/201)) ([fac4966](https://github.com/kunchenguid/no-mistakes/commit/fac496655b8a374add73583e47a9b8d7133c0034))

## [1.19.1](https://github.com/kunchenguid/no-mistakes/compare/v1.19.0...v1.19.1) (2026-05-15)


### Bug Fixes

* **pipeline:** compact PR testing evidence ([#198](https://github.com/kunchenguid/no-mistakes/issues/198)) ([edc7dc3](https://github.com/kunchenguid/no-mistakes/commit/edc7dc315dcac735a7d269c7b4eaf96edbf6c8b9))

## [1.19.0](https://github.com/kunchenguid/no-mistakes/compare/v1.18.3...v1.19.0) (2026-05-15)


### Features

* **pipeline:** surface intent-based test evidence ([#196](https://github.com/kunchenguid/no-mistakes/issues/196)) ([7d2880d](https://github.com/kunchenguid/no-mistakes/commit/7d2880deb49d6e47cb691d68f068e102801a711b))

## [1.18.3](https://github.com/kunchenguid/no-mistakes/compare/v1.18.2...v1.18.3) (2026-05-14)


### Bug Fixes

* **tui:** show completed fix progress counts ([#194](https://github.com/kunchenguid/no-mistakes/issues/194)) ([fdc4dea](https://github.com/kunchenguid/no-mistakes/commit/fdc4dea9b2944460f54be6447570d580b868c325))

## [1.18.2](https://github.com/kunchenguid/no-mistakes/compare/v1.18.1...v1.18.2) (2026-05-14)


### Bug Fixes

* **pipeline:** commit no-config lint agent fixes ([#192](https://github.com/kunchenguid/no-mistakes/issues/192)) ([c900d8d](https://github.com/kunchenguid/no-mistakes/commit/c900d8d9a80e63c6182a36b5860b42d91f7da9e9))

## [1.18.1](https://github.com/kunchenguid/no-mistakes/compare/v1.18.0...v1.18.1) (2026-05-14)


### Bug Fixes

* **pipeline:** report live fix progress accurately ([#190](https://github.com/kunchenguid/no-mistakes/issues/190)) ([e85c2b6](https://github.com/kunchenguid/no-mistakes/commit/e85c2b6b25a83e2aa38bac56aea461ae854aec36))

## [1.18.0](https://github.com/kunchenguid/no-mistakes/compare/v1.17.1...v1.18.0) (2026-05-14)


### Features

* **tui:** show fixed finding progress in pipeline rows ([#189](https://github.com/kunchenguid/no-mistakes/issues/189)) ([45f6aba](https://github.com/kunchenguid/no-mistakes/commit/45f6aba6762df563bac631fc26e352a7caa1f00d))


### Bug Fixes

* **intent:** read camelCase tool file paths ([#187](https://github.com/kunchenguid/no-mistakes/issues/187)) ([3ddbb1e](https://github.com/kunchenguid/no-mistakes/commit/3ddbb1e3b136f6924a604eaa4802362b34600a92))

## [1.17.1](https://github.com/kunchenguid/no-mistakes/compare/v1.17.0...v1.17.1) (2026-05-13)


### Bug Fixes

* **conventional:** infer release types for non-conventional titles ([#185](https://github.com/kunchenguid/no-mistakes/issues/185)) ([1b84147](https://github.com/kunchenguid/no-mistakes/commit/1b8414745658952d42cfc3af3a1b9a6f4f86960f))

## [1.17.0](https://github.com/kunchenguid/no-mistakes/compare/v1.16.0...v1.17.0) (2026-05-11)


### Features

* **pipeline:** prefer root-cause fix prompts ([#182](https://github.com/kunchenguid/no-mistakes/issues/182)) ([865e9ba](https://github.com/kunchenguid/no-mistakes/commit/865e9ba748219f90be0583be643f04984b622a9c))


### Bug Fixes

* **pipeline:** use workdir for intent git state ([#183](https://github.com/kunchenguid/no-mistakes/issues/183)) ([8b9238f](https://github.com/kunchenguid/no-mistakes/commit/8b9238f05212e8c5a47a12023b42c3f56a26772c))

## [1.16.0](https://github.com/kunchenguid/no-mistakes/compare/v1.15.0...v1.16.0) (2026-05-10)


### Features

* **cli:** add usage stats command ([#179](https://github.com/kunchenguid/no-mistakes/issues/179)) ([990b26c](https://github.com/kunchenguid/no-mistakes/commit/990b26cb8e405d83f03d33e16be7a46fe4d3ea20))

## [1.15.0](https://github.com/kunchenguid/no-mistakes/compare/v1.14.0...v1.15.0) (2026-05-10)


### Features

* **pipeline:** add intent extraction as a pipeline step ([#175](https://github.com/kunchenguid/no-mistakes/issues/175)) ([78c9e7d](https://github.com/kunchenguid/no-mistakes/commit/78c9e7d7d4d5d5e836f3508e193dc9c86ac04e2e))
* **pipeline:** add PR intent sections ([#177](https://github.com/kunchenguid/no-mistakes/issues/177)) ([d7eb261](https://github.com/kunchenguid/no-mistakes/commit/d7eb261b27b04425e70fd41e155ef817d7240152))


### Bug Fixes

* **pipeline:** handle orphaned intent base SHAs ([#178](https://github.com/kunchenguid/no-mistakes/issues/178)) ([e710e3c](https://github.com/kunchenguid/no-mistakes/commit/e710e3c2e582397c418ae905a44ff30809ca57a2))

## [1.14.0](https://github.com/kunchenguid/no-mistakes/compare/v1.13.1...v1.14.0) (2026-05-09)


### Features

* **intent:** add transcript-based intent extraction ([#173](https://github.com/kunchenguid/no-mistakes/issues/173)) ([5f0301d](https://github.com/kunchenguid/no-mistakes/commit/5f0301d75b061978c16dd6938f5646339c9e57ef))

## [1.13.1](https://github.com/kunchenguid/no-mistakes/compare/v1.13.0...v1.13.1) (2026-05-06)


### Bug Fixes

* **daemon:** refresh managed hooks during recovery ([#171](https://github.com/kunchenguid/no-mistakes/issues/171)) ([13a91c2](https://github.com/kunchenguid/no-mistakes/commit/13a91c275d7549d82fc4ede2d4cc4a0d3336e67b))

## [1.13.0](https://github.com/kunchenguid/no-mistakes/compare/v1.12.1...v1.13.0) (2026-05-05)


### Features

* **cli:** add per-run pipeline step skipping ([#169](https://github.com/kunchenguid/no-mistakes/issues/169)) ([5992808](https://github.com/kunchenguid/no-mistakes/commit/59928089f495d7c1731cd784202fa7131098337f))

## [1.12.1](https://github.com/kunchenguid/no-mistakes/compare/v1.12.0...v1.12.1) (2026-05-04)


### Bug Fixes

* kunchenguid/no-mistakes[#164](https://github.com/kunchenguid/no-mistakes/issues/164) ([#167](https://github.com/kunchenguid/no-mistakes/issues/167)) ([7c1e01c](https://github.com/kunchenguid/no-mistakes/commit/7c1e01c52b155b45a69d44ef7f05d23173f0bf26))

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
