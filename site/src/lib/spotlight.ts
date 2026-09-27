// What the spotlight after the first day of events shows. One hardcoded entry for now.
// The component only ever sees these fields, so rotating entries later
// means changing what this returns, not touching the page or the component.
export type SpotlightEntry = {
  label: string;
  text: string;
  cta: string;
  url: string;
  // Disclosure shown next to "Spotlight". Set it whenever the entry is
  // something the site's own maker built, so the plug is never passed off as
  // a neutral pick.
  byline?: string;
};

export const spotlight: SpotlightEntry = {
  label: 'Built with Compendium',
  text: 'Every tool this site is built with is pinned in one config file.',
  cta: 'See how',
  url: 'https://compendium.ilean.me?utm_source=htxdev&utm_medium=spotlight',
  byline: 'By the maker of htxdev',
};
