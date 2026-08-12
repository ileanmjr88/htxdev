# htxdev

What's happening in Houston tech today.

Houston's tech events are scattered across Meetup, Google Calendar, Luma, and a
handful of WordPress sites. There's no single place to look, so people miss
things that were happening two blocks away. htxdev reads the feeds groups
already publish and puts them in one place.

Organizers don't have to do anything differently. If your group publishes a
calendar anywhere, that's the integration.

## Status

**Early. Not live yet.** Phase 1 of 8: the domain types and the first feed
decoder are done and tested. There is no site and no API yet.

| | |
|---|---|
| ✅ Domain types, Ion (WordPress) decoder | done |
| ⬜ Registry, live fetch, worker pool | next |
| ⬜ iCalendar decoder (Meetup, Google Calendar) | |
| ⬜ SQLite store, dedupe, venue resolution | |
| ⬜ The site | |
| ⬜ Public JSON API | |

## How it works

```
data/sources.yaml  →  fetch  →  decode  →  dedupe  →  htxdev.db  →  the site
   the curated                  one decoder            permanent
   list of groups               per format             event history
```

Two decoders cover every source: iCalendar, which Meetup and Google Calendar
both emit, and The Events Calendar's JSON API, which WordPress sites expose.
Adding a group is an entry in a YAML file, not code.

The database is committed to the repo on purpose. It's the permanent record of
every event ever seen, which is what makes "this group has met every Wednesday
for two years" a thing the site can know.

## Adding a group

Open a pull request against [`data/sources.yaml`](data/sources.yaml). One entry:

```yaml
  - slug: houston-example
    name: Houston Example Group
    url: https://example.org
    category: dev
    verified_by:
    verified_at:
    sources:
      - kind: ics
        url: https://www.meetup.com/houston-example/events/ical/
        priority: 10
        enabled: true
```

For a Meetup group, take the slug out of the URL and append `/events/ical/`.
That's the whole onboarding cost.

`kind` is `ics` for iCalendar or `tribe` for a WordPress site running The Events
Calendar (its feed lives at `/wp-json/tribe/events/v1/events`).

Venues are usually discovered from the event data. You only need to add one by
hand when its name shows up under several spellings and needs canonicalizing.

## Verification and privacy

**A new source's events are not published until a human has reviewed that
source's first sync.** `verified_by` is deliberately blank on every new entry,
and filling it in is the act of verification: it means a person confirmed the
group is real and the feed is legitimately theirs.

This matters more than it sounds. Public community calendars sometimes carry
entries that were never meant to be published, like personal appointments on a
shared group account. Public event APIs sometimes return contact details in
fields nobody thinks about. Ingesting a feed naively republishes all of it.

So the pipeline is built to not do that. Feed decoders read only the fields
they need, and nothing reaches the site until someone has looked at it.

**If you organize a group and you'd rather not be listed, open an issue and
you'll be removed.** Same if you'd like to be added but your calendar isn't
public.

## Development

The toolchain is pinned with [Compendium](https://compendium.ilean.me):

```bash
compendium install
source <(compendium activate)
```

Or use Go 1.26.1 directly (see [`go.mod`](go.mod)); everything except `make lint`
works without Compendium.

```bash
make check     # fmt + vet + test, run this before committing
make test      # go test ./...
make coverage  # coverage report
make lint      # golangci-lint (needs an activated shell)
make help      # all targets
```

Tests are offline. Decoders take an `io.Reader`, so they're fed fixtures in
[`internal/source/testdata/`](internal/source/testdata) and can't reach the
network even by accident.

## Layout

```
cmd/htxdev          sync and serve commands
internal/core       domain types; imports only the standard library
internal/source     one decoder per feed format, wire types stay private
internal/registry   sources.yaml loader
internal/store      SQLite
internal/api        HTTP handlers
data/sources.yaml   the curated list of groups, venues, and feeds
web/                the site
```

`internal/core` is imported by everything and imports nothing. Feed-specific
types never leave `internal/source`, which is what keeps WordPress and
iCalendar quirks out of the rest of the system.

Not all of these exist yet. See Status.

## License

Not yet chosen.
