// @ts-check
import { defineConfig } from 'astro/config';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  // Static. The whole point of the pipeline is that everything is decided
  // before a reader arrives: sync writes the database, export writes
  // data/events.json, and this renders it. Nothing here runs per request.
  output: 'static',

  // Settled by D12 on 2026-09-18: this is a Cloudflare Worker custom domain,
  // declared in wrangler.jsonc so Cloudflare owns the DNS record. Used here
  // for absolute URLs in a sitemap or feed, neither of which exists yet.
  site: 'https://htxdev.ilean.me',

  build: { format: 'directory' },

  // Tailwind 4 is a Vite plugin rather than an Astro integration, and its
  // config lives in CSS. See the @theme block in src/styles/global.css.
  vite: { plugins: [tailwindcss()] },
});
