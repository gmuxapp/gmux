import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import mermaid from 'astro-mermaid';

export default defineConfig({
  site: 'https://gmux.app',
  integrations: [
    starlight({
      title: 'gmux',
      description: 'Keep tabs on every AI agent, test runner, and long-running process across your machines.',
      logo: {
        light: './src/assets/logo-light.svg',
        dark: './src/assets/logo-dark.svg',
        replacesTitle: true,
      },
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/gmuxapp/gmux' },
        { icon: 'discord', label: 'Discord', href: 'https://discord.gg/Mg6EJHFZxu' },
      ],
      customCss: ['./src/styles/custom.css'],
      sidebar: [
        { label: 'Getting Started', slug: 'getting-started' },
        { label: 'Changelog', slug: 'changelog' },
        {
          label: 'Guides',
          items: [
            { label: 'Using the UI', slug: 'using-the-ui' },
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
          autogenerate: { directory: 'reference' },
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
