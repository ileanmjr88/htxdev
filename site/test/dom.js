// A DOM small enough to read, faithful enough to run the real filter script.
//
// The site ships one inline script that decides which events a reader sees.
// There is no browser in a build, so without this it would be the only
// user-facing behaviour in the project with no test at all, which does not sit
// well beside a Go suite that gets mutation-tested.
//
// Two rules keep it honest. It runs the script extracted from the BUILT page,
// not a copy, so it cannot drift. And its fixture is derived from
// data/events.json using the same Chicago-date grouping index.astro uses: the
// first version grouped by the UTC date instead and silently put nine events on
// the wrong day, which made every assertion meaningless while they all passed.
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const TZ = 'America/Chicago';
const dayKey = new Intl.DateTimeFormat('en-CA', {
  year: 'numeric', month: '2-digit', day: '2-digit', timeZone: TZ,
});

export function loadFixture(root) {
  const html = readFileSync(resolve(root, 'site/dist/index.html'), 'utf8');
  const data = JSON.parse(readFileSync(resolve(root, 'data/events.json'), 'utf8'));

  const scripts = [...html.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)].map((m) => m[1]);
  const script = scripts.find((s) => s.includes('htxdev-hidden-groups'));
  if (!script) throw new Error('filter script not found in the built page');

  const days = new Map();
  for (const e of data.events) {
    const key = dayKey.format(new Date(e.start));
    if (!days.has(key)) days.set(key, []);
    days.get(key).push(e.group.slug);
  }
  const counts = new Map();
  for (const e of data.events) counts.set(e.group.slug, (counts.get(e.group.slug) ?? 0) + 1);

  const jumps = [...html.matchAll(/data-jump="([0-9-]+)"/g)].map((m) => m[1]);
  const pageDays = [...html.matchAll(/id="day-([0-9-]+)"/g)].map((m) => m[1]);

  return {
    script,
    total: data.events.length,
    days: [...days].map(([key, groups]) => ({ key, groups })),
    groups: [...counts].map(([slug, count]) => ({ slug, count })).sort((a, b) => b.count - a.count),
    jumps,
    pageDays,
  };
}

class El {
  constructor(tag, attrs = {}) {
    this.tag = tag; this.attrs = { ...attrs }; this.children = []; this.parent = null;
    this.dataset = {}; this._hidden = false; this.textContent = '';
    this.id = attrs.id ?? '';
    this._classes = new Set((attrs.class ?? '').split(/\s+/).filter(Boolean));
    this.classList = {
      toggle: (c, on) => { on ? this._classes.add(c) : this._classes.delete(c); },
      contains: (c) => this._classes.has(c),
    };
    this._listeners = [];
    for (const [k, v] of Object.entries(attrs)) {
      if (k.startsWith('data-')) {
        this.dataset[k.slice(5).replace(/-(\w)/g, (_, c) => c.toUpperCase())] = v;
      }
    }
  }
  get hidden() { return this._hidden; }
  set hidden(v) { this._hidden = !!v; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return this.attrs[k]; }
  append(c) { c.parent = this; this.children.push(c); return c; }
  *walk() { for (const c of this.children) { yield c; yield* c.walk(); } }
  // Only the selectors the script actually uses. Anything else throws rather
  // than quietly returning nothing, so a new selector cannot pass by default.
  matches(sel) {
    switch (sel) {
      case 'article[data-group]': return this.tag === 'article' && 'group' in this.dataset;
      case 'article[data-group]:not([hidden])':
        return this.tag === 'article' && 'group' in this.dataset && !this._hidden;
      case 'section[data-day]': return this.tag === 'section' && 'day' in this.dataset;
      case 'button[data-filter]': return this.tag === 'button' && 'filter' in this.dataset;
      case 'a[data-jump]': return this.tag === 'a' && 'jump' in this.dataset;
      default: throw new Error(`unsupported selector: ${sel}`);
    }
  }
  querySelectorAll(sel) { return [...this.walk()].filter((e) => e.matches(sel)); }
  querySelector(sel) { return this.querySelectorAll(sel)[0] ?? null; }
  closest(sel) { let n = this; while (n) { if (n.matches?.(sel)) return n; n = n.parent; } return null; }
  addEventListener(_type, fn) { this._listeners.push(fn); }
  click(target) {
    const ev = {
      target: target ?? this, defaultPrevented: false,
      preventDefault() { this.defaultPrevented = true; },
    };
    for (const fn of this._listeners) fn(ev);
    return ev;
  }
  scrollIntoView() { this._scrolled = true; }
}

export function buildDOM(fx) {
  const root = new El('body');
  const bar = new El('div', { id: 'filters' }); root.append(bar);
  const status = new El('p', { id: 'filter-status' }); root.append(status);
  const nav = new El('nav', { id: 'jumps' }); root.append(nav);

  bar.append(new El('button', { 'data-filter': 'all' }));
  for (const g of fx.groups) bar.append(new El('button', { 'data-filter': g.slug }));
  for (const j of fx.jumps) nav.append(new El('a', { 'data-jump': j }));
  for (const d of fx.days) {
    const sec = new El('section', { 'data-day': d.key, id: `day-${d.key}` });
    root.append(sec);
    for (const g of d.groups) sec.append(new El('article', { 'data-group': g }));
  }

  const byId = { filters: bar, 'filter-status': status, jumps: nav };
  return {
    root, bar, status, nav,
    document: {
      getElementById: (id) => byId[id] ?? null,
      querySelectorAll: (s) => root.querySelectorAll(s),
    },
  };
}

export function memoryStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
  };
}

// A browser in private mode, or with site data blocked, throws here rather
// than returning null. An unguarded read would take the whole listing down.
export function hostileStorage() {
  return {
    getItem() { throw new Error('storage denied'); },
    setItem() { throw new Error('storage denied'); },
  };
}

export function run(fx, env, storage) {
  new Function('document', 'localStorage', fx.script)(env.document, storage);
  return env;
}
