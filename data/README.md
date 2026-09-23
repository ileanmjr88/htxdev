# The registry

The curated list of Houston tech groups and where their events come from. These
files are the product; the code is plumbing.

```
data/groups/<slug>.yaml   one group and its feeds
data/venues/<slug>.yaml   one curated venue
data/rejects.yaml         individual events that must not publish
```

The filename is the slug. There is no `slug:` key to keep in step with it, and
it has to be lowercase letters, digits and single hyphens. A group's slug ends
up in URLs and in the database, so pick it once.

## Adding a group

Copy a file that looks like yours, rename it, and edit it. For a Meetup group,
copy the slug out of the URL and append `/events/ical/`; that is the whole
onboarding cost.

```yaml
name: Houston Code and Coffee
url: https://www.meetup.com/houston-code-and-coffee/
category: dev
venue: improving-houston   # optional; a filename in venues/
verified_by:
verified_at:
sources:
  - kind: ics              # ics, tribe, html or bevy
    url: https://www.meetup.com/houston-code-and-coffee/events/ical/
    priority: 10
    enabled: true
```

`go test ./internal/registry` loads every file and reports every problem at
once, and CI runs it on every pull request. Unknown keys are errors, so a typo
cannot quietly disable a source.

## Verification

`verified_by` and `verified_at` start blank. Filling them in is the act of
verification: a human confirming this group is real and its feed is
legitimately theirs. Until then the group's events stay `pending` and do not
publish. Change these only in a pull request; the commit author is the
provenance.

This matters concretely. Houston Linux User Group's public calendar contains
personal appointments ("Dental", "Family visit") mixed in with real events.
Review a source's first sync before trusting it. See
[CONTRIBUTING.md](../CONTRIBUTING.md#vouch-for-a-group) for how.

## Priority

Lower wins when the same event appears in two feeds. A group's own feed (10)
beats a venue's listing of it (50), because the organizer is authoritative
about their own event.

## Names

A group answers to its slug, its `name` and every entry in `aliases`. That is
how another feed's organizer field resolves to a group, so two groups cannot
claim the same name; the loader rejects it, comparing names the way the
pipeline matches them (case, curly quotes and dashes folded). Venues work the
same way among themselves.

## Venues

Venues are mostly discovered from event data and need no file. Curate one when
its name needs canonicalizing, it shows up under several spellings, or a group
meets somewhere its feed never names.

## Deliberately excluded

Recorded so nobody re-adds them or repeats the research.

- **PyTexas virtual meetup** (meetup.com/pytexas-virtual-meetup/). The
  statewide online one, first Tuesday of the month in Discord. Not excluded for
  being Austin-based, which an earlier version of this note said and was wrong
  about: it is virtual and belongs to no city. Excluded because PyHou
  cross-posts it, so adding both would publish every one twice.

  That note also said no Houston Python group could be found. One could: it is
  meetup.com/python-14 (`groups/pyhou.yaml`). The slug is a legacy numeric one,
  so no amount of guessing at names would have reached it. PyTexas links it
  from pytexas.org/meetup/local-meetups/, which is where to look next time
  rather than at Meetup's search.

- **meetup.com/houston-pyladies**. A real group, and NOT Houston PyLadies. It
  is named just "PyLadies", has no events, and is a different group from
  meetup.com/Houston_PyLadies, which is the live one. Recorded because the
  hyphenated spelling is the one somebody would guess at.

- **Other Houston-area Python groups PyTexas lists, not yet looked at**:
  meetup.com/katy-python-coders, meetup.com/houston-data-science. Katy is
  Houston metro; the data science one may be more data than dev.

- **Eventbrite (any group)**. robots.txt disallows `/rss/`, `/atom/` and
  `/events/rss/`.

- **LinkedIn Events (any group)**. robots.txt prohibits automated access
  outright, not just by path. Several Houston groups register attendees there,
  Side Project Society among them, and that is where their start times live.
  Not readable, and not worth asking LinkedIn about.

- **sideprojectsociety.com as an `html` source**. NOT excluded, just not
  ready. Its event pages publish a date with no start time, and the index needs
  one request per event. See `groups/side-project-society.yaml`; the first move
  is asking the organizer to put the time in the datetime attribute they
  already emit, not writing a decoder that has to guess one.

- **Discord-only groups**. Scheduled events require a bot token and per-server
  membership. Unreachable, and htxdev has no way to list an event by hand yet.
