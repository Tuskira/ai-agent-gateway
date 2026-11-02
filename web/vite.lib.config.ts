import { copyFileSync, mkdirSync, readFileSync } from 'node:fs'
import path from 'node:path'
import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig, type Plugin } from 'vite'

// Library build of the console (`npm run build:lib`), published as
// @tuskira/gateway-web. Separate from vite.config.ts, which builds the
// embedded console into ../internal/api/ui/dist. Output goes to dist-lib/:
//   index.js   ESM bundle of src/ (every dependency stays external)
//   style.css  Tailwind output + design tokens, one file
//   fonts.css  optional self-hosted Inter / JetBrains Mono (src/fonts.css)
// Type declarations come from tsconfig.lib.json (see the build:lib script).

const pkg = JSON.parse(
  readFileSync(path.resolve(import.meta.dirname, 'package.json'), 'utf-8'),
) as {
  dependencies?: Record<string, string>
  peerDependencies?: Record<string, string>
}

// Peer dependencies must be the host's copies; runtime dependencies are
// installed next to this package, so none of them are bundled either.
const externalNames = [
  ...Object.keys(pkg.peerDependencies ?? {}),
  ...Object.keys(pkg.dependencies ?? {}),
]
const external = externalNames.map(
  (name) => new RegExp(`^${name.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&')}(/.*)?$`),
)

// fonts.css @imports @fontsource/* CSS; those packages are runtime
// dependencies, so the consumer's bundler resolves the imports itself.
function emitFontsCss(): Plugin {
  return {
    name: 'emit-fonts-css',
    closeBundle() {
      mkdirSync(path.resolve(import.meta.dirname, 'dist-lib'), { recursive: true })
      copyFileSync(
        path.resolve(import.meta.dirname, 'src/fonts.css'),
        path.resolve(import.meta.dirname, 'dist-lib/fonts.css'),
      )
    },
  }
}

export default defineConfig({
  plugins: [react(), tailwindcss(), emitFontsCss()],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, './src'),
    },
  },
  build: {
    outDir: 'dist-lib',
    emptyOutDir: true,
    cssCodeSplit: false,
    copyPublicDir: false,
    lib: {
      entry: path.resolve(import.meta.dirname, 'src/index.ts'),
      formats: ['es'],
      fileName: () => 'index.js',
      cssFileName: 'style',
    },
    rollupOptions: { external },
  },
})
