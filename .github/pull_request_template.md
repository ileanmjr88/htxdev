## What this changes

<!-- One or two sentences. Link the issue if there is one: "Closes #12". -->

## Kind of change

<!-- Tick one and fill in its section. Delete the others. -->

- [ ] Add a group
- [ ] Verify a group
- [ ] Reject an event
- [ ] Add or fix a venue
- [ ] Code or docs

### Add a group

- [ ] One new file, `data/groups/<slug>.yaml`, named for the group's slug
- [ ] `verified_by` and `verified_at` left blank (adding a group and vouching for it are separate acts)
- [ ] `aliases` lists any other names the group appears under in other feeds
- [ ] `venue:` set only if the group always meets in one place

How you found the feed:

### Verify a group

- [ ] `verified_by` is your GitHub handle and `verified_at` is today
- [ ] You read the group's events in the feed, not just checked that it loads

Your basis (read the feed / attend / run the group / know the organizer):

### Reject an event

- [ ] Keyed on the fingerprint, never the title
- [ ] The reason is generic and does not repeat the private entry

### Code or docs

- [ ] `make check` and `make lint` pass
- [ ] If the site changed: `npm --prefix site run build` and `npm --prefix site test` pass
- [ ] Comments explain why, and any comment recording a decision you changed is updated

<!-- Please do not include data/events.json or htxdev.db. The sync job updates them. -->
