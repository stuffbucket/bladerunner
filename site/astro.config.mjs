import { defineConfig } from 'astro/config';

// GitHub Pages PROJECT site for stuffbucket/bladerunner
// served at https://stuffbucket.github.io/bladerunner
export default defineConfig({
  site: 'https://stuffbucket.github.io',
  base: '/bladerunner',
  output: 'static',
  trailingSlash: 'ignore',
  build: {
    assets: 'assets',
  },
});
