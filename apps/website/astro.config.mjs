import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  site: 'https://gmux.dev',
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
        {
          label: 'Getting Started',
          items: [
            { label: 'Introduction', slug: 'introduction' },
            { label: 'Quick Start', slug: 'quick-start' },
            { label: 'Remote Access', slug: 'remote-access' },
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
            { label: 'pi', slug: 'integrations/pi' },
          ],
        },
        {
          label: 'Develop',
          items: [
            { label: 'Adapter Architecture', slug: 'develop/adapter-architecture' },
            { label: 'Writing an Adapter', slug: 'develop/writing-adapters' },
          ],
        },
      ],
      // Disable default homepage — we use a custom one
      components: {},
    }),
  ],
});
