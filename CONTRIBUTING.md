# Contributing

The most useful thing you can do here is not code.

htxdev is a curated list of Houston tech groups with a pipeline attached. The
list is the part that matters and the part that goes stale; the pipeline mostly
looks after itself. So the contributions that help most, roughly in order:

1. **Vouch for a group** so its events can publish
2. **Add a group** that is missing
3. **Tell us a group is wrong**, gone, or does not want to be listed
4. **Fix a venue** that shows up under two names or has no address
5. Code

None of the first four require knowing Go, and three of them are a form.

---

## Vouch for a group

Events from a group nobody has vouched for are collected and stored but **never
published**. That is the whole safety model, and it is why a group can sit in
the registry for weeks showing nothing.

Open issues are tracked as
[`verification`](../../issues?q=is%3Aissue+label%3Averification), one per group.
Each carries what is already known about that feed.

### What vouching actually is

Not a code review. The feed already decodes and the URL already resolves. You
are saying two things:

1. **This group is real**, and this feed is legitimately theirs rather than
   something found by search that looked plausible.
2. **Its output is safe to republish.** Look at what it publishes, not at
   whether it parses.

The second one is not paranoia. Houston Linux User Group runs its public
calendar from an account that is also somebody's personal calendar, so
`Family visit` and `Dental` sat between real meetings. Verifying that group
meant reading all 110 events on the feed, and the second one was three weeks
from appearing on the site when it was found.

**A title filter does not work.** That was tried and disproven on this exact
feed: real events appear as `Houston Linux - User Meeting`, `HLUG Social`,
`Tuesday Meeting @Sesh` and `Social Gathering`, with no common prefix, and
`Lunch meeting` is indistinguishable from a private entry. A person reading it
is the only control there is.

### How to look

```bash
source <(compendium activate)
make run                  # sync every source
make site-dev-preview     # localhost:4321, including unverified groups
```

Or in the terminal:

```bash
./bin/htxdev sync -n -v   # every event, writing nothing
```

`make run` also warns about any event carrying neither a venue nor a
description, which is the shape a personal appointment takes on a shared
calendar. It is a prompt to look, not a filter.

### How to record it

Two lines in the group's file, `data/groups/<slug>.yaml`:

```yaml
verified_by: your-github-handle
verified_at: 2026-09-18
```

Both, or the loader rejects it. Then open a pull request; the commit author is
the provenance.

**Say what your basis was** in a comment if it is not "I read the feed". Knowing
the organizer personally is a stronger claim than reading output, and a feed
with no events yet cannot be reviewed at all. Both are fine. A future reader
should not have to guess which one was made.

### If something should not publish

Add its fingerprint to [`data/rejects.yaml`](data/rejects.yaml):

```yaml
  - fingerprint: "ics:abc123@google.com"
    reason: Personal appointment on a shared group calendar, not a group event
    rejected_by: your-github-handle
    rejected_at: 2026-09-18
```

**Keyed on the fingerprint, never the title.** A title-keyed list would contain
the titles it exists to suppress, which for the case above means writing
somebody's private calendar entry into a public file in order to stop
publishing somebody's private calendar entry. Keep the reason generic for the
same reason.

Find a fingerprint with `./bin/htxdev sync -n -v`, or:

```sh
sqlite3 -readonly htxdev.db "SELECT fingerprint, title, starts_at FROM events ORDER BY starts_at"
```

A reject applies the moment it is in the file, whether or not `rejected_by` is
filled in. That is the opposite of how `verified_by` behaves and deliberate:
both blank fields fail closed, because both point at "do not publish".

---

## Add a group

[Use the form.](.github/ISSUE_TEMPLATE/add-group.yml) You do not need to know
what an `.ics` file is; a link to the group is enough.

If you would rather send a pull request, see the example in the
[README](README.md#adding-a-group). Three things are not guessable:

- **`category` belongs to the group, not the event.** Feeds mostly do not say
  what kind of thing they are, and groups know what they are.
- **`aliases` are load-bearing.** They are how two feeds describing one meeting
  are recognised as one event. Ion writes the Linux group as
  `Houston Linux User's Group` with a curly apostrophe, and that single line is
  the only reason those events deduplicate.
- **`venue:` is a default, not an override.** Set it only when a group meets in
  one place. A feed that names somewhere wins, because the feed knows about the
  week the meeting moved. Leave it blank if the group rotates: a wrong venue
  sends somebody to the wrong building.

---

## Add or fix a venue

[Use the form.](.github/ISSUE_TEMPLATE/add-venue.yml) Most venues need no entry
at all; they are discovered from event data and get a row automatically.

Curate one when a name needs settling: the same building arriving under several
spellings, a feed that gives no address, or a group meeting somewhere its
calendar never names.

---

## Ask a group to publish a calendar

Some groups publish nothing machine-readable. The best fix is asking them, not
writing a scraper, and it is usually a smaller ask than it sounds: often a
start time in a `<time datetime="...">` attribute their template already emits.

Each group's file in [`data/groups/`](data/groups/) records whether it is in that
position, and [`data/README.md`](data/README.md#deliberately-excluded) lists the
sources ruled out and exactly what was already tried, so nobody repeats the research.

---

## Code

```bash
source <(compendium activate)   # from the repo root, always
make check                      # fmt + vet + test under -race
make lint                       # needs the activated shell
```

For the site:

```bash
npm --prefix site install     # once
npm --prefix site run build   # the tests read the built page
npm --prefix site test
```

The front page ships one inline script that decides which events a reader sees,
and `site/test/` runs it against the built HTML with a small DOM stub. It reads
the real `data/events.json`, so it fails if the fixture stops describing the
page rather than passing on stale assumptions.

Both must be clean. The same thing runs on your pull request: `.github/workflows/check.yml`
does the Go build, `gofmt`, `vet`, the race detector and lint, plus the site
build and a check that `/api/v1/events.json` still matches `data/events.json`
byte for byte.

You do not need to run a sync or regenerate anything. `data/events.json` and
`htxdev.db` are committed, so the site and the API build from the repository
with no network. The sync job updates them twice a day on its own; if your pull
request changes them, that is a merge conflict waiting to happen and probably
not what you meant.

A few things about this codebase that will save you time:

- **Decoders take an `io.Reader` and cannot reach the network.** Everything is
  fixture-driven and offline. Keep it that way; the fixtures are real payloads
  in `internal/source/testdata/`.
- **Wire types stay private to their package.** Feeds carry fields nobody wants
  in a database, including contact details. A narrow hand-written struct is how
  those stay out, which is why you will not find `map[string]any` here.
- **Comments explain why, not what.** Most of the interesting ones record a
  decision and the data behind it. If you change the decision, change the
  comment.
- **Tests are mutation-tested.** Breaking the code on purpose to check the tests
  notice has caught more real problems here than the tests found on their own,
  including two branches that turned out to be dead rather than untested.
- **Measure before you design.** Nearly every wrong turn in this project came
  from reasoning about what a feed probably contains. Nearly every good decision
  came from reading the payload first. The fixtures are committed so you can.
