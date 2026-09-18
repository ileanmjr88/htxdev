import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';

// One loader, two consumers: the page that renders the events and the endpoint
// that republishes them at /api/v1/events.json. That is the same rule the Go
// side follows, where internal/api/wire.go owns the shape for both the server
// and the export. A second copy of this logic is a second thing to keep in
// step with the first.

// Read at build time with fs rather than importing the JSON, because the file
// lives outside this Astro project and Vite's filesystem allow-list would
// otherwise have to be widened to reach it. It is also honest about when this
// happens: the site is static, so build time is the only time there is.
//
// Resolved against the working directory rather than import.meta.url, which
// points at the bundled chunk in dist/ once Astro has built it and sends the
// relative path somewhere that does not exist. Both candidates are here
// because `npm run build` inside site/ and `npm --prefix site run build` from
// the repo root are both reasonable and have different working directories.
function candidates(): string[] {
  return [
    process.env.HTXDEV_EVENTS,
    'data/events.json',
    '../data/events.json',
  ].filter((p): p is string => Boolean(p)).map((p) => resolve(p));
}

function locate(): string {
  const tried = candidates();
  const file = tried.find((p) => existsSync(p));
  if (!file) {
    throw new Error(
      `no events file found. Looked in:\n  ${tried.join('\n  ')}\n` +
      `Run "htxdev export" first, or "htxdev export -preview" to include ` +
      `events from groups nobody has verified yet.`,
    );
  }
  return file;
}

/**
 * The file's bytes, unparsed.
 *
 * The endpoint serves these verbatim rather than re-encoding a parsed object,
 * so what Cloudflare returns is byte-identical to what `htxdev serve` returns
 * and to what is committed in the repository. Re-encoding would reorder keys
 * and reformat timestamps, and the two APIs would drift without either one
 * being wrong.
 */
export function readEventsRaw(): string {
  return readFileSync(locate(), 'utf8');
}

export interface Feed {
  generated_at: string;
  preview?: boolean;
  events: Event[];
}

export interface Event {
  id: string;
  title: string;
  excerpt?: string;
  start: string;
  end?: string;
  all_day?: boolean;
  group: { slug: string; name: string; url?: string };
  venue?: { name: string; address?: string; city?: string; state?: string; zip?: string; url?: string };
  room?: string;
  url?: string;
  register_url?: string;
  categories?: string[];
  virtual?: boolean;
  sources?: number;
  pending?: boolean;
}

export function readEvents(): Feed {
  const data = JSON.parse(readEventsRaw()) as Feed;
  return { ...data, events: data.events ?? [] };
}
