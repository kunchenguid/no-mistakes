import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";

export default defineConfig({
  site: "https://kunchenguid.github.io",
  base: "/no-mistakes",
  integrations: [
    starlight({
      title: "no-mistakes",
      social: {
        github: "https://github.com/kunchenguid/no-mistakes",
        discord: "https://discord.gg/Wsy2NpnZDu",
        "x.com": "https://x.com/kunchenguid",
      },
      sidebar: [
        {
          label: "Start Here",
          items: [
            { label: "Introduction", slug: "start-here/introduction" },
            { label: "Quick Start", slug: "start-here/quick-start" },
            { label: "Installation", slug: "start-here/installation" },
          ],
        },
        {
          label: "Concepts",
          items: [
            { label: "The Gate Model", slug: "concepts/gate-model" },
            { label: "Pipeline", slug: "concepts/pipeline" },
            { label: "Auto-Fix Loop", slug: "concepts/auto-fix" },
            { label: "Daemon & Worktrees", slug: "concepts/daemon" },
          ],
        },
        {
          label: "Guides",
          items: [
            { label: "Configuration", slug: "guides/configuration" },
            { label: "Choosing an Agent", slug: "guides/agents" },
            { label: "Provider Integration", slug: "guides/provider-integration" },
            { label: "Setup Wizard", slug: "guides/setup-wizard" },
            { label: "Using the TUI", slug: "guides/tui" },
            { label: "Troubleshooting", slug: "guides/troubleshooting" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "CLI Commands", slug: "reference/cli" },
            { label: "Pipeline Steps", slug: "reference/pipeline-steps" },
            { label: "Global Config", slug: "reference/global-config" },
            { label: "Repo Config", slug: "reference/repo-config" },
            { label: "Environment Variables", slug: "reference/environment" },
          ],
        },
      ],
    }),
  ],
});
