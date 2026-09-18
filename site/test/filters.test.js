// Tests for the inline filter and jump script on the front page.
//
// Run against the BUILT page, so `npm --prefix site run build` has to have
// happened first. That is deliberate: testing a copy of the script would test
// the copy.
import { strict as assert } from 'node:assert';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { loadFixture, buildDOM, memoryStorage, hostileStorage, run } from './dom.js';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const fx = loadFixture(ROOT);

const boot = (storage = memoryStorage()) => run(fx, buildDOM(fx), storage);
const cards = (e) => e.document.querySelectorAll('article[data-group]');
const shown = (e) => cards(e).filter((c) => !c.hidden).length;
const days = (e) => e.document.querySelectorAll('section[data-day]');
const visibleDays = (e) => days(e).filter((d) => !d.hidden).length;
const chip = (e, slug) =>
  e.bar.querySelectorAll('button[data-filter]').find((b) => b.dataset.filter === slug);
const jumpLinks = (e) => e.document.querySelectorAll('a[data-jump]');

// The fixture has to describe the page it is testing, or every assertion below
// is measuring something else. This caught exactly that: grouping by the UTC
// date rather than the Chicago one put nine events on the wrong day.
test('the fixture matches the built page', () => {
  assert.deepEqual(fx.days.map((d) => d.key), fx.pageDays,
    'fixture days differ from the day sections in the built HTML');
  assert.ok(fx.jumps.every((j) => fx.days.some((d) => d.key === j)),
    'a jump link points at a day that does not exist');
});

test('everything is visible before anything is filtered', () => {
  const e = boot();
  assert.equal(shown(e), fx.total);
  assert.equal(e.status.textContent, '');
  assert.equal(chip(e, 'all').getAttribute('aria-pressed'), 'true');
});

test('hiding a group hides exactly that group', () => {
  const e = boot();
  const big = fx.groups[0];
  e.bar.click(chip(e, big.slug));
  assert.equal(shown(e), fx.total - big.count);
  assert.equal(chip(e, big.slug).getAttribute('aria-pressed'), 'false');
  assert.equal(chip(e, 'all').getAttribute('aria-pressed'), 'false');
  assert.equal(e.status.textContent, `Showing ${fx.total - big.count} of ${fx.total} events.`);
});

// A date heading with nothing under it reads as a rendering bug.
test('a day whose events are all filtered out hides its heading', () => {
  const e = boot();
  const big = fx.groups[0];
  const soleOwner = fx.days.filter((d) => d.groups.every((g) => g === big.slug)).length;
  e.bar.click(chip(e, big.slug));
  assert.equal(visibleDays(e), fx.days.length - soleOwner);
});

test('toggling a group back restores it', () => {
  const e = boot();
  e.bar.click(chip(e, fx.groups[0].slug));
  e.bar.click(chip(e, fx.groups[0].slug));
  assert.equal(shown(e), fx.total);
  assert.equal(visibleDays(e), fx.days.length);
});

test('filtering everything out says so instead of looking broken', () => {
  const e = boot();
  for (const g of fx.groups) e.bar.click(chip(e, g.slug));
  assert.equal(shown(e), 0);
  assert.equal(visibleDays(e), 0);
  assert.equal(e.status.textContent, 'No events match. Every group is filtered out.');
});

test('All clears every filter', () => {
  const e = boot();
  for (const g of fx.groups) e.bar.click(chip(e, g.slug));
  e.bar.click(chip(e, 'all'));
  assert.equal(shown(e), fx.total);
  assert.equal(e.status.textContent, '');
});

test('a choice survives a reload', () => {
  const store = memoryStorage();
  const a = boot(store);
  a.bar.click(chip(a, fx.groups[0].slug));
  assert.equal(shown(boot(store)), fx.total - fx.groups[0].count);
});

// State is the set of HIDDEN slugs, so a group added later is visible to
// everyone. A stored slug for a group that has LEFT would otherwise sit in
// storage forever hiding nothing.
test('a stored slug for a departed group is dropped', () => {
  const store = memoryStorage();
  store.setItem('htxdev-hidden-groups', JSON.stringify([fx.groups[0].slug, 'group-that-left']));
  const e = boot(store);
  assert.equal(shown(e), fx.total - fx.groups[0].count);
  assert.ok(!JSON.parse(store.getItem('htxdev-hidden-groups')).includes('group-that-left'));
});

test('a browser that blocks storage still works', () => {
  const e = boot(hostileStorage());
  assert.equal(shown(e), fx.total, 'the listing should render even if storage throws');
  e.bar.click(chip(e, fx.groups[0].slug));
  assert.equal(shown(e), fx.total - fx.groups[0].count, 'filtering should work without persistence');
});

test('jump links start live and undimmed', () => {
  const e = boot();
  assert.equal(jumpLinks(e).length, fx.jumps.length);
  assert.ok(jumpLinks(e).every((l) => l.getAttribute('aria-disabled') === 'false'));
});

// The anchor is correct on its own when its target is visible, so the script
// must not intercept it and break history and native scrolling.
test('an unfiltered jump link keeps native anchor behaviour', () => {
  const e = boot();
  const link = jumpLinks(e)[0];
  assert.equal(link.click(link).defaultPrevented, false);
});

// An anchor to a display:none element scrolls nowhere, which reads as a dead
// link rather than as a filter doing its job.
test('a jump link whose day is hidden lands on the next visible one', () => {
  const e = boot();
  const link = jumpLinks(e)[0];
  const target = link.dataset.jump;
  const owners = fx.days.find((d) => d.key === target).groups;
  for (const slug of new Set(owners)) e.bar.click(chip(e, slug));

  const section = days(e).find((s) => s.dataset.day === target);
  assert.ok(section.hidden, 'test setup should have hidden the target day');

  assert.equal(link.click(link).defaultPrevented, true);
  const landed = days(e).find((s) => s._scrolled);
  assert.ok(landed, 'nothing was scrolled to');
  assert.ok(!landed.hidden, 'scrolled to a hidden heading');
  assert.ok(landed.dataset.day >= target, 'scrolled backwards past the target');
});

test('jump links go inert when nothing is left after them', () => {
  const e = boot();
  for (const g of fx.groups) e.bar.click(chip(e, g.slug));
  assert.ok(jumpLinks(e).every((l) => l.getAttribute('aria-disabled') === 'true'));
  assert.ok(jumpLinks(e).every((l) => l.classList.contains('pointer-events-none')));
  assert.equal(jumpLinks(e)[0].click(jumpLinks(e)[0]).defaultPrevented, true);

  e.bar.click(chip(e, 'all'));
  assert.ok(jumpLinks(e).every((l) => l.getAttribute('aria-disabled') === 'false'));
});
