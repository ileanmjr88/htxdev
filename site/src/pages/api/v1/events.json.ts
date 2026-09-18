import type { APIRoute } from 'astro';
import { readEventsRaw } from '../../../lib/events';

// The same path `htxdev serve` answers, served as a static file instead.
//
// D12 settled this: the data changes twice a day and every response is the
// same bytes for everybody, so there is nothing for a process to decide at
// request time. Cloudflare serves this from its edge for free and handles
// ETag and 304 itself. The Go server in internal/api is what generates these
// bytes and what runs locally; it does not have to be running for the API to
// exist.
//
// Prerendered, so this executes at build time and emits dist/api/v1/events.json.
export const prerender = true;

export const GET: APIRoute = () =>
  new Response(readEventsRaw(), {
    headers: {
      'Content-Type': 'application/json; charset=utf-8',
      // Cache-Control and CORS come from public/_headers, which applies to
      // what Cloudflare serves. Setting them here would only affect the dev
      // server, and having them in two places is how they stop matching.
    },
  });
