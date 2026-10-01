// Docs site for tusk-ai-secured-gateway, published at community.tuskira.ai
// by .github/workflows/docs.yml (GitHub Pages). The content is the repo's
// own docs/ folder, unchanged: this directory only holds the site shell.
//
// markdown.format is "md" on purpose: the docs are plain CommonMark that
// GitHub also renders, and they use <placeholder> tokens and {braces} in
// prose that MDX would try to parse as JSX/expressions.
// @ts-check
import { themes as prismThemes } from 'prism-react-renderer';

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'AI Agent Gateway',
  tagline: 'One authenticated, logged front door for MCP tool servers and LLM providers',
  favicon: 'img/tuskira_icon.svg',

  url: 'https://community.tuskira.ai',
  baseUrl: '/',
  trailingSlash: false,

  organizationName: 'Tuskira',
  projectName: 'tusk-ai-secured-gateway',

  // A broken link is a build failure, so CI catches it before it ships.
  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',

  markdown: {
    format: 'md',
    hooks: {
      onBrokenMarkdownLinks: 'throw',
    },
  },

  i18n: { defaultLocale: 'en', locales: ['en'] },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          path: '../docs',
          routeBasePath: '/',
          sidebarPath: './sidebars.js',
          editUrl: 'https://github.com/Tuskira/tusk-ai-secured-gateway/edit/main/',
          exclude: ['**/.gitkeep'],
          showLastUpdateTime: false,
        },
        blog: false,
        pages: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      }),
    ],
  ],

  themes: [
    [
      '@easyops-cn/docusaurus-search-local',
      /** @type {import('@easyops-cn/docusaurus-search-local').PluginOptions} */
      ({
        hashed: true,
        docsRouteBasePath: '/',
        indexBlog: false,
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      colorMode: { respectPrefersColorScheme: true },
      navbar: {
        logo: {
          alt: 'Tuskira',
          src: 'img/tuskira-logotype.svg',
          srcDark: 'img/tuskira-logotype-white.svg',
          href: '/',
          width: 100,
          height: 100,
        },
        items: [
          { to: '/', label: 'Gateway Docs', position: 'left' },
          { href: 'https://github.com/Tuskira/tusk-ai-secured-gateway/tree/main/examples', label: 'Examples', position: 'left' },
          { href: 'https://app.tuskira.ai/documentation', label: 'Tuskira Help', position: 'left' },
          { href: 'https://github.com/Tuskira/tusk-ai-secured-gateway', label: 'GitHub', position: 'right' },
          { type: 'search', position: 'right' },
        ],
      },
      footer: {
        style: 'dark',
        links: [
          {
            items: [
              { label: 'Source', href: 'https://github.com/Tuskira/tusk-ai-secured-gateway' },
              { label: 'Releases', href: 'https://github.com/Tuskira/tusk-ai-secured-gateway/releases' },
              { label: 'Contributing', href: 'https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/CONTRIBUTING.md' },
              { label: 'Security policy', href: 'https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/SECURITY.md' },
              { label: 'tuskira.ai', href: 'https://www.tuskira.ai' },
            ],
          },
        ],
        copyright: `© ${new Date().getFullYear()} Tuskira, Inc. Licensed under the Apache License 2.0. AI Agent Gateway and Tuskira are trademarks of Tuskira, Inc. — <a href="https://github.com/Tuskira/tusk-ai-secured-gateway/blob/main/TRADEMARKS.md">Trademark policy</a>`,
      },
      prism: {
        theme: prismThemes.github,
        darkTheme: prismThemes.dracula,
        additionalLanguages: ['go', 'bash', 'yaml', 'json', 'sql', 'toml', 'docker'],
      },
    }),
};

export default config;
