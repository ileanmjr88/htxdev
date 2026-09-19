# htxdev

What's happening in Houston tech today.

Houston's tech events are scattered across Meetup, Google Calendar, Luma, and a
handful of WordPress sites. There's no single place to look, so people miss
things that were happening two blocks away. htxdev reads the feeds groups
already publish and puts them in one place.

Organizers don't have to do anything differently. If your group publishes a
calendar anywhere, that's the integration.

## Status

**Working, deploying.** Eleven groups, seven verified, around seventy events a
run. The site and the API build from a committed database, and the sync runs
itself.

| | |
|---|---|
| ✅ Domain types, Ion (WordPress) decoder | |
| ✅ Registry, live fetch, bounded worker pool | |
| ✅ iCalendar decoder, recurrence expansion | |
| ✅ SQLite store, `first_seen`, the publishing gate | |
| ✅ Dedupe, venue resolution, excerpts, categories | |
| ✅ `events.json` export and the site | |
| ✅ Public JSON API | `internal/api`, served two ways |
| ✅ Scheduled sync (GitHub Actions) | twice daily |
| ⬜ Connect the Cloudflare project | one dashboard step |

## How it works

```
data/sources.yaml → fetch → decode → dedupe → htxdev.db ─┬→ data/events.json → the site
   the curated               one decoder      permanent  │      committed         /
   list of groups            per format       history    └→ htxdev serve     /api/v1/events.json
```

Three decoders cover every source. iCalendar, which Meetup and Google Calendar
both emit; The Events Calendar's JSON API, which WordPress sites expose; and
one HTML reader for a single group that publishes no calendar at all but does
publish `<time datetime="...">` on its meetings page. Adding a group is an entry
in a YAML file, not code.

Dedupe is keyed on the group and the start instant, never the title, because
the same meeting arrives as "Houston Linux User Group" from one feed and
"Houston Linux - Ion User Meeting" from another. Two groups meeting at the same
hour stay two events, because that happens every week.

The database is committed to the repo on purpose. It's the permanent record of
every event ever seen, which is what makes "this group has met every Wednesday
for two years" a thing the site can know.

The site is one page: every event still ahead, grouped by day, with jump links
for this week, next week and each month after that, and a toggle per group that
a reader's browser remembers. One inline script does the filtering over the
events already in the HTML, so the page works with JavaScript off and the
anchors work before the script runs.

## Running it

```bash
source <(compendium activate)   # from the repo root; see Development
make run                        # fetch every source into htxdev.db
make site-dev                   # the site on localhost:4321
```

`make run` prints a per-source summary and writes the database. `make sync-dry`
does the same and writes nothing. `make db` opens the database read-only.

`make serve` runs the API on `localhost:8080`. It serves the same bytes
Cloudflare serves, from the database rather than from a file, and Ctrl-C shuts
it down without dropping an in-flight request.

## Deployment

Three pieces, and none of them run a server.

**GitHub Actions** syncs twice a day, commits `htxdev.db` and
`data/events.json` if anything changed, and pushes. `.github/workflows/sync.yml`
holds one `concurrency` group, so two runs can never write the database at
once. A push made with `GITHUB_TOKEN` does not trigger other workflows, which
is what stops it retriggering itself.

**Cloudflare Workers** builds the site from that push and serves it from the
edge. Static assets, no Worker script: `site/wrangler.jsonc` has an `assets`
block and no `main`. Cloudflare's guidance since 2025 is that new projects
start on Workers rather than Pages, and static asset requests are free.

**The API is a file.** `site/src/pages/api/v1/events.json.ts` is prerendered at
build time, so `/api/v1/events.json` is a static asset with the same bytes the
Go server returns. Cloudflare handles ETag and 304 itself; `site/public/_headers`
carries the CORS and caching rules that `internal/api/middleware.go` sets in Go.

That makes `htxdev serve` the development server and the reference
implementation rather than production infrastructure. The data changes twice a
day and every response is identical for everybody, so there is nothing for a
process to decide at request time. It is one command away from being deployed
if that ever stops being true.

### Connecting the Cloudflare project

Once, in the dashboard: Workers → Create → connect this repository, set the
root directory to `site/`, the build command to `npm run build`, and the deploy
command to `npx wrangler deploy`. Node comes from `site/.node-version`, which
CI checks against `compendium.toml` so the deployed build and the local one
cannot drift apart.

## Adding a group

Easiest is the [issue template](.github/ISSUE_TEMPLATE/add-group.yml), which
asks for what it needs and nothing else. If you would rather send a pull
request, it is one entry in [`data/sources.yaml`](data/sources.yaml):

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

Verification is tracked as [issues labelled
`verification`](../../issues?q=is%3Aissue+label%3Averification), one per group
still waiting. Each carries what is already known about that feed so nobody
redoes the research.

**If you organize a group and you'd rather not be listed, open an issue and
you'll be removed.** Same if you'd like to be added but your calendar isn't
public. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Development

The toolchain is pinned with [Compendium](https://compendium.ilean.me):

```bash
compendium install
source <(compendium activate)
```

This pins Go, Node and golangci-lint. Run it **from the repo root**:
`compendium activate` reads `compendium.toml` from the working directory, so
from a subdirectory it fails silently and you build on whatever happens to be
on your PATH. Every npm target in the Makefile uses `--prefix site` for that
reason.

Or use Go 1.26.1 and Node 22 directly (see [`go.mod`](go.mod) and
[`compendium.toml`](compendium.toml)); everything except `make lint` works
without Compendium.

```bash
make check      # fmt + vet + test under -race, run this before committing
make test       # go test ./...
make test-race  # go test -race ./...
make coverage   # coverage report
make lint       # golangci-lint (needs an activated shell)
make site       # export events.json and build the site
make help       # all targets
```

Tests are offline. Decoders take an `io.Reader`, so they're fed fixtures in
[`internal/source/testdata/`](internal/source/testdata) and can't reach the
network even by accident.

The front page's script is the one piece of user-facing behaviour no Go test
can reach, so it has its own suite:

```bash
npm --prefix site run build   # the tests read the built page
npm --prefix site test
```

CI runs both suites on every pull request, plus lint, the API asset check, and
a check that `compendium.toml` and `site/.node-version` still pin the same
Node.
[CONTRIBUTING.md](CONTRIBUTING.md) has what's expected of a change.

## Layout

```
cmd/htxdev          sync and export commands
internal/core       domain types; imports only the standard library
internal/source     one decoder per feed format, wire types stay private
internal/registry   sources.yaml and rejects.yaml loaders
internal/fetch      HTTP, concurrency, the Fetcher interface
internal/normalize  resolution, dedupe, venues, excerpts
internal/store      SQLite
internal/api        HTTP handlers and the JSON contract
data/sources.yaml   the curated list of groups, venues, and feeds
data/rejects.yaml   individual events that must not publish
site/               the Astro site and the static API
site/test/          the front page script, run against the built page
.github/workflows/  sync on a schedule, check on every push
```

`internal/core` is imported by everything and imports nothing. Feed-specific
types never leave `internal/source`, which is what keeps WordPress and
iCalendar quirks out of the rest of the system.

`internal/api/wire.go` owns the JSON shape for both consumers: the server
serves it and `htxdev export` writes it to `data/events.json`. The static file
is the API's response precomputed, not a second format to keep in step.

## License

[MIT](LICENSE).

That covers the code. The event listings in `htxdev.db` and
`data/events.json` come from feeds the groups publish themselves and belong to
those groups, which is why being removed from them takes an issue and not a
license argument. See Verification and privacy above.
