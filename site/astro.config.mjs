// @ts-check
import { defineConfig } from 'astro/config';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  // Static. The whole point of the pipeline is that everything is decided
  // before a reader arrives: sync writes the database, export writes
  // data/events.json, and this renders it. Nothing here runs per request.
  output: 'static',

  // htxdev.ilean.me is named in the architecture and D12 has not settled
  // where it is hosted. site is only used for absolute URLs in the sitemap
  // and feeds, so being wrong here costs nothing until one of those exists.
  site: 'https://htxdev.ilean.me',

  build: { format: 'directory' },

  // Tailwind 4 is a Vite plugin rather than an Astro integration, and its
  // config lives in CSS. See the @theme block in src/styles/global.css.
  vite: { plugins: [tailwindcss()] },
});
