import { defineConfig } from "astro/config";
import mermaid from "astro-mermaid";
import starlight from "@astrojs/starlight";

export default defineConfig({
  site: "https://kunchenguid.github.io",
  base: "/no-mistakes",
  integrations: [
    mermaid({ enableLog: false }),
    starlight({
      title: "git push no-mistakes",
      customCss: ["./src/styles/custom.css"],
      social: {
        github: "https://github.com/kunchenguid/no-mistakes",
        discord: "https://discord.gg/Wsy2NpnZDu",
        "x.com": "https://x.com/kunchenguid",
      },
      sidebar: [
        {
          label: "Start here",
          items: [
            { label: "Introduction", slug: "start-here/introduction" },
            { label: "Quick start", slug: "start-here/quick-start" },
            { label: "Installation", slug: "start-here/installation" },
          ],
        },
        {
          label: "Concepts",
          items: [
            { label: "The gate model", slug: "concepts/gate-model" },
            { label: "Pipeline", slug: "concepts/pipeline" },
            { label: "Automatic repair", slug: "concepts/auto-fix" },
            { label: "Daemon and worktrees", slug: "concepts/daemon" },
          ],
        },
        {
          label: "Guides",
          items: [
            { label: "Configuration", slug: "guides/configuration" },
            { label: "Routing and agents", slug: "guides/agents" },
            { label: "Migrating to routing", slug: "guides/migrating-to-routing" },
            { label: "Provider integration", slug: "guides/provider-integration" },
            { label: "Setup wizard", slug: "guides/setup-wizard" },
            { label: "Using the TUI", slug: "guides/tui" },
            { label: "Troubleshooting", slug: "guides/troubleshooting" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "CLI commands", slug: "reference/cli" },
            { label: "Pipeline steps", slug: "reference/pipeline-steps" },
            { label: "Global config", slug: "reference/global-config" },
            { label: "Repo config", slug: "reference/repo-config" },
            { label: "Routing", slug: "reference/routing" },
            { label: "Environment variables", slug: "reference/environment" },
          ],
        },
      ],
    }),
  ],
});
