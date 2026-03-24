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
      ],
      customCss: ['./src/styles/custom.css'],
      sidebar: [
        { label: 'Changelog', slug: 'changelog' },
        {
          label: 'Getting Started',
          items: [
            { label: 'Introduction', slug: 'introduction' },
            { label: 'Quick Start', slug: 'quick-start' },
            { label: 'Using the UI', slug: 'using-the-ui' },
            { label: 'Remote Access', slug: 'remote-access' },
            { label: 'Configuration', slug: 'configuration' },
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
          label: 'Integrations',
          items: [
            { label: 'Claude Code', slug: 'integrations/claude-code' },
            { label: 'Codex', slug: 'integrations/codex' },
            { label: 'pi', slug: 'integrations/pi' },
          ],
        },
        {
          label: 'Develop',
          items: [
            { label: 'State Management', slug: 'develop/state-management' },
            { label: 'Session Schema', slug: 'develop/session-schema' },
            { label: 'Adapter Architecture', slug: 'develop/adapter-architecture' },
            { label: 'Writing an Adapter', slug: 'develop/writing-adapters' },
            { label: 'Integration Tests', slug: 'develop/integration-tests' },
            { label: 'Terminal Data Pipeline', slug: 'develop/terminal-data-pipeline' },
          ],
        },
        {
          label: 'Planned',
          items: [
            { label: 'Session Persistence', slug: 'planned/session-persistence' },
            { label: 'Probes', slug: 'planned/probes' },
            { label: 'Notifications', slug: 'planned/notifications' },
            { label: 'Mobile Notifications', slug: 'planned/mobile-notifications' },
          ],
        },
      ],
      components: {
        Head: './src/components/Head.astro',
      },
    }),
    mermaid(),
  ],
});
