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
            { label: "Getting Started", slug: "getting-started" },
            { label: "How It Works", slug: "how-it-works" },
          ],
        },
        {
          label: "Guides",
          items: [
            { label: "Configuration", slug: "guides/configuration" },
            { label: "Pipeline Steps", slug: "guides/pipeline-steps" },
            { label: "Auto-Fix", slug: "guides/auto-fix" },
            { label: "Agents", slug: "guides/agents" },
            { label: "TUI", slug: "guides/tui" },
            { label: "Daemon", slug: "guides/daemon" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "CLI Commands", slug: "reference/cli" },
            { label: "Repo Config", slug: "reference/repo-config" },
            { label: "Global Config", slug: "reference/global-config" },
          ],
        },
      ],
    }),
  ],
});
