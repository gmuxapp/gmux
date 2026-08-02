import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import mermaid from 'astro-mermaid';

export default defineConfig({
  site: 'https://gmux.app',
  redirects: {
    // Folded into the CLI reference (raw-session scripting) and
    // Orchestrating agents (semantic agent surface).
    '/integrations/scripts-and-agents': '/reference/cli/',
    // Old short URL used in the v1.6.0 release notes.
    '/scripts-and-agents': '/reference/cli/',
  },
  integrations: [
    starlight({
      title: 'gmux',
      description: 'The control plane for AI agents. Humans and agents start, watch, steer, and delegate coding-agent sessions from one place.',
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/gmuxapp/gmux' },
        { icon: 'discord', label: 'Discord', href: 'https://discord.gg/Mg6EJHFZxu' },
      ],
      customCss: ['./src/styles/custom.css'],
      sidebar: [
        { label: 'Getting Started', slug: 'getting-started' },
        { label: 'Migrating to 2.0', slug: 'migrating-to-2' },
        { label: 'Changelog', slug: 'changelog' },
        {
          label: 'Guides',
          items: [
            { label: 'Using the UI', slug: 'using-the-ui' },
            { label: 'Orchestrating Agents', slug: 'orchestrating-agents' },
            { label: 'Devcontainers', slug: 'devcontainers' },
            { label: 'Multi-Machine Sessions', slug: 'multi-machine' },
            { label: 'Configuration', slug: 'configuration' },
            { label: 'Remote Access', slug: 'remote-access' },
            { label: 'Running in Docker', slug: 'running-in-docker' },
            { label: 'Troubleshooting', slug: 'troubleshooting' },
          ],
        },
        {
          label: 'Concepts',
          items: [
            { label: 'Architecture', slug: 'architecture' },
            { label: 'Adapters', slug: 'adapters' },
            { label: 'Security', slug: 'security' },
          ],
        },
        {
          label: 'Reference',
          items: [
            { label: 'CLI', slug: 'reference/cli' },
            { label: 'Environment variables', slug: 'reference/environment' },
            { label: 'File paths', slug: 'reference/file-paths' },
            { label: 'host.toml', slug: 'reference/host-toml' },
            { label: 'Interface stability', slug: 'reference/stability' },
            { label: 'Settings', slug: 'reference/settings' },
            { label: 'Theme', slug: 'reference/theme' },
            { label: 'URLs and filters', slug: 'reference/urls' },
          ],
        },
        {
          label: 'Integrations',
          autogenerate: { directory: 'integrations' },
        },
        {
          label: 'Develop',
          collapsed: true,
          autogenerate: { directory: 'develop' },
        },
        {
          label: 'Planned',
          collapsed: true,
          autogenerate: { directory: 'planned' },
        },
      ],
      components: {
        Head: './src/components/Head.astro',
        ThemeProvider: './src/components/ThemeProvider.astro',
        ThemeSelect: './src/components/ThemeSelect.astro',
      },
    }),
    mermaid(),
  ],
});
