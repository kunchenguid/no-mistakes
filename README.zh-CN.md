<h1 align="center"><code>git push no-mistakes</code></h1>
<p align="center">
  <a href="https://github.com/kunchenguid/no-mistakes/actions/workflows/release.yml"
    ><img
      alt="Release"
      src="https://img.shields.io/github/actions/workflow/status/kunchenguid/no-mistakes/release.yml?style=flat-square&label=release"
  /></a>
  <a href="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows-blue?style=flat-square"
    ><img
      alt="Platform"
      src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux%20%7C%20Windows-blue?style=flat-square"
  /></a>
  <a href="https://x.com/kunchenguid"
    ><img
      alt="X"
      src="https://img.shields.io/badge/X-@kunchenguid-black?style=flat-square"
  /></a>
  <a href="https://discord.gg/Wsy2NpnZDu"
    ><img
      alt="Discord"
      src="https://img.shields.io/discord/1439901831038763092?style=flat-square&label=discord"
  /></a>
</p>

<h3 align="center">干掉所有 slop，开出干净的 PR。</h3>

<p align="center"><a href="README.md">English</a> · <strong>简体中文</strong></p>

<p align="center">
  <img src="https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/demo.gif" alt="no-mistakes demo" width="800" />
</p>

`no-mistakes` 在你真实的远端前面放了一个本地 git 代理。
把分支推给 `no-mistakes` 而不是 `origin`，它就会在一个用完即弃的 worktree 里跑一条 AI 驱动的校验流水线。
所有本地及推送前 gate 都必须通过，Push 才会发布 Verify 认证的确切提交。
PR 创建和托管 CI 都发生在发布之后。

你会得到：

- 一条隔离的流水线，绝不阻塞你的工作副本
- 按语义用途（Purpose）路由的模型调用，可在 OpenAI 与 Anthropic 之间做提供商故障切换
- 一个 `/no-mistakes` skill，让你的编码 agent 完成任务并过网关，或直接为已提交的工作过网关
- 每次修复都先应用、再经确定性检查和独立验证，之后才允许发布任何内容
- 替你开出干净的 PR 并盯好 CI，需要人拍板的事仍由你决定

完整文档：<https://kunchenguid.github.io/no-mistakes/>

## 工作原理

```
        你的分支
            │  git push no-mistakes
            ▼
   ┌────────────────────────────────────────────────┐
   │  用完即弃的 worktree —— 你的工作原地不动        │
   │  intent → rebase → review → test → document    │
   │  → lint → verify → push → PR → CI              │
   └────────────────────────────────────────────────┘
            │  本地及推送前 gate 通过后才会进入 Push
            ▼
        已发布认证提交，已开出干净 PR，托管 CI 变绿
```

流水线永远按同样的十个步骤运行：intent（意图）→ rebase（变基）→ review（评审）→ test（测试）→ document（文档）→ lint（静态检查）→ verify（验证）→ push（推送）→ PR → CI。
每个步骤要么自行通过，要么停下来留下一条 finding。
系统会替你修复可安全修复的 finding；任何触及你意图的事项都会等待你作出决定。
push 步骤只负责传输 verify 步骤认证过的那一个确切提交。
在所有本地及推送前 gate 变绿之前，任何东西都不会到达配置的推送目标。

## 模型调用如何选定

每一次模型调用都始于一个语义用途（Purpose），例如首次评审或 PR 撰写。
Purpose 选定一条由能力档位（Profile）组成的有限路由（Route），每个 Profile 按优先顺序列出各提供商的候选（Candidate）。
一旦某次调用出现已分类的运行故障（例如配额耗尽或服务中断），该候选所属提供商的熔断器就会在本次运行期间保持打开，并由同一 Profile 内的备用候选接管。
当所有候选都不可用时，这次调用会直接以失败收场（fail closed），而不是悄悄降级。
系统会将每条 finding 作为持久谱系（lineage）追踪：先由新的修复执行者处理，再运行确定性检查，最后由独立验证者验证，并沿 Route 逐级升级，直到该 finding 被解决或以失败关闭（fail closed）的方式终止。
完整契约见[路由参考](https://kunchenguid.github.io/no-mistakes/reference/routing/)。

## 安装

```sh
curl -fsSL https://raw.githubusercontent.com/kunchenguid/no-mistakes/main/docs/install.sh | sh
```

你需要 `git`，以及 `PATH` 上至少一个已接入路由的 runner CLI：`codex` 或 `claude`。
Windows、Go install 以及从源码构建的说明，见[安装指南](https://kunchenguid.github.io/no-mistakes/start-here/installation/)。

## 快速上手

```sh
$ no-mistakes init
  ✓ Gate initialized

    repo  /Users/you/src/my-repo
    gate  no-mistakes → /Users/you/.no-mistakes/repos/abc123def456.git
  remote  git@github.com:you/my-repo.git
   skill  /no-mistakes installed for agents at user level

  Push through the gate with:
  git push no-mistakes <branch>

$ git checkout my-branch

# 在分支里干点活……

$ git push no-mistakes
  * Pipeline started

  Run no-mistakes to review.

$ no-mistakes
# 打开当前运行的 TUI
```

若要通过 GitHub fork 贡献代码，请让 `origin` 保持指向父仓库，并使用 `no-mistakes init --fork-url <your-fork-url>` 初始化。

在 TUI 里你逐条处理 finding。
流水线会自行修复安全的 finding，并对每次修复做独立验证；需要人拍板的事项会让运行停下来，由你 approve（批准）、fix（修复）或 skip（跳过）。
所有本地及推送前 gate 变绿后，Push 才会把认证过的提交转发到配置的推送目标。
PR 会在发布后创建，托管 CI 随后检查已发布的提交。
你不需要手动运行 `git push origin`，也不用手写 PR 正文。
希望让编码智能体以无界面方式驱动同一流程？
用 `/no-mistakes`（见下文）。

## 触发网关的三种方式

每一处改动都走同一条流水线。
改动就绪时，挑一个最贴合你当下工作方式的入口：

- `git push no-mistakes` —— 显式的 Git 路径：把已提交的分支推给网关 remote，而不是 `origin`
- `no-mistakes` —— TUI：改动后运行它（无需先提交），向导会根据需要处理分支和提交，再完成推送并连接到该运行；`no-mistakes -y` 会自动完成这些步骤
- `/no-mistakes` —— agent skill：用 `/no-mistakes <task>` 让编码 agent 完成一个任务并过网关，或用裸 `/no-mistakes` 为已提交的工作过网关；它让流水线修复安全的 finding，并在任何需要人拍板的地方停下来问你

`no-mistakes init` 会为 Claude Code 及其他 agent 安装 `/no-mistakes` skill。
在底层，该技能会驱动 `no-mistakes axi`，后者是同一审批流程的非交互式 TOON 接口。

完整的首次运行走查见[快速上手](https://kunchenguid.github.io/no-mistakes/start-here/quick-start/)。

## 开发

```sh
make build   # 构建 bin/no-mistakes（带版本信息）
make test    # 运行 go test -race ./...（不含 e2e 套件）
make e2e     # 运行带标签的端到端智能体旅程测试套件
make e2e-record # 智能体通信格式变更时，重新录制 e2e 测试夹具
make lint    # 检查生成的 skill 是否漂移，并跑 go vet ./...
make skill   # 重新生成已提交的 no-mistakes skill 文件
make fmt     # 运行 gofmt -w .
make demo    # 重新生成 demo.gif 和 demo.mp4（需要 vhs 和 ffmpeg）
make docs    # 在 docs/dist 构建 Astro 文档站
```

完整 target 列表见 `Makefile`。

`make e2e-record` 会用真实的 `claude`、`codex`、`opencode` CLI 覆盖 `internal/e2e/fixtures/`，会消耗真实 API 额度，提交前应当审查。

## Star 历史

<a href="https://www.star-history.com/?repos=kunchenguid%2Fno-mistakes&type=date&legend=top-left">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/chart?repos=kunchenguid/no-mistakes&type=date&theme=dark&legend=top-left" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/chart?repos=kunchenguid/no-mistakes&type=date&legend=top-left" />
   <img alt="Star History Chart" src="https://api.star-history.com/chart?repos=kunchenguid/no-mistakes&type=date&legend=top-left" />
 </picture>
</a>
