package normalize

import (
	"strings"
	"testing"
)

func TestExcerpt(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// Verbatim shape from Ion. The opening paragraph is a registration
			// link and says nothing, so the excerpt has to start after it.
			name: "leading register link is skipped",
			in: `<p><em><strong>Register Here: </strong><a href="https://luma.com/u9ycnl37">https://luma.com/u9ycnl37</a></em></p>` +
				`<p class="">Join JERA and Mitsubishi Heavy Industries for an evening of candid conversation.</p>`,
			want: "Join JERA and Mitsubishi Heavy Industries for an evening of candid conversation.",
		},
		{
			name: "entities are decoded",
			in:   `<p>&quot;Quoted&quot; &amp; entities &#8211; decoded</p>`,
			want: "\"Quoted\" & entities – decoded",
		},
		{
			name: "nested markup is flattened",
			in:   `<p><em><strong>Bold</strong> and <a href="/x">linked</a></em> text</p>`,
			want: "Bold and linked text",
		},
		{
			name: "list items become separate blocks",
			in:   `<ul><li>First item</li><li>Second item</li></ul>`,
			want: "First item",
		},
		{
			name: "an image contributes nothing",
			in:   `<p><img src="/banner.png" alt="Banner"> Real text follows.</p>`,
			want: "Real text follows.",
		},
		{
			// Every ICS feed sends plain text, where a blank line is the only
			// paragraph signal there is.
			name: "plain text splits on a blank line",
			in:   "Houston Code and Coffee\n\nCode & Coffee is a friendly, low-pressure meetup.",
			want: "Houston Code and Coffee",
		},
		{
			name: "single newlines do not split",
			in:   "One line\nstill the same paragraph.",
			want: "One line still the same paragraph.",
		},
		{
			name: "whitespace is collapsed",
			in:   "<p>Lots   of\n\t  space</p>",
			want: "Lots of space",
		},
		{
			name: "script contents never become text",
			in:   `<p>Real text.</p><script>var x = "not text";</script>`,
			want: "Real text.",
		},
		{"empty", "", ""},
		{"only a link", `<p><a href="https://example.com">https://example.com</a></p>`, ""},
		{"only markup", `<div><img src="/x.png"></div>`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := excerpt(tc.in); got != tc.want {
				t.Errorf("excerpt() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Ion's descriptions run to 6.5KB. An excerpt is a card, so it gets cut, and
// it gets cut at a word boundary.
func TestExcerptTruncatesAtAWordBoundary(t *testing.T) {
	long := "<p>" + strings.Repeat("Houston technology community ", 40) + "</p>"
	got := excerpt(long)

	if len(got) > excerptLimit+len("…") {
		t.Errorf("excerpt is %d bytes, want at most %d", len(got), excerptLimit)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("excerpt = %q, want a trailing ellipsis", got)
	}
	// The cut must not land inside a word.
	trimmed := strings.TrimSuffix(got, "…")
	if !strings.HasSuffix(trimmed, "community") && !strings.HasSuffix(trimmed, "technology") && !strings.HasSuffix(trimmed, "Houston") {
		t.Errorf("excerpt = %q, want it to end on a whole word", got)
	}
}

func TestExcerptLeavesShortTextAlone(t *testing.T) {
	const short = "A short description."
	if got := excerpt("<p>" + short + "</p>"); got != short {
		t.Errorf("excerpt = %q, want %q with no ellipsis", got, short)
	}
}

func TestIsMostlyLink(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"Register Here: https://luma.com/u9ycnl37?lm_source=embed", true},
		{"https://example.com/a/very/long/path/that/dominates", true},
		{"Join us at the Ion on Thursday for a talk about Go.", false},
		{"We will meet at https://meet.google.com/abc but the real detail is that this is a long paragraph of actual prose about the event itself.", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isMostlyLink(tc.in); got != tc.want {
			t.Errorf("isMostlyLink(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// Zero-width characters are not whitespace as far as strings.Fields is
// concerned, because they are format characters rather than spaces. Ion's
// descriptions carry them, pasted in from whatever wrote the copy, and 2 of 55
// live excerpts began with one. Without this they survive into the published
// JSON as an invisible first character that makes an excerpt look like it
// starts with a space.
//
// Written as escape sequences on purpose. A literal U+FEFF byte in a Go source
// file is a compile error, "illegal byte order mark", which is how the first
// draft of this test announced itself.
func TestExcerptStripsZeroWidthCharacters(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"zero width space", "<p>\u200bJoin Halliburton Labs for Pitch Day.</p>", "Join Halliburton Labs for Pitch Day."},
		{"byte order mark", "<p>\ufeffLeading BOM.</p>", "Leading BOM."},
		{"soft hyphen mid-word", "<p>Hous\u00adton</p>", "Houston"},
		{"zero width joiner", "<p>a\u200db</p>", "ab"},
		{"several, including a trailing one", "<p>\u200bReal text.\u200b</p>", "Real text."},
		{"a real space is still a space", "<p>Real  text.</p>", "Real text."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := excerpt(tc.in)
			if got != tc.want {
				t.Errorf("excerpt() = %q, want %q", got, tc.want)
			}
			for _, r := range []rune{'\u200b', '\u200c', '\u200d', '\ufeff', '\u00ad'} {
				if strings.ContainsRune(got, r) {
					t.Errorf("excerpt() = %q, which still contains %U", got, r)
				}
			}
		})
	}
}

// Meetup writes its ICS descriptions in Markdown and sends them as plain text,
// so the syntax arrives verbatim and would be published verbatim. This is not
// a Markdown parser: the paragraph is about to be cut to 220 characters, so
// the job is "do not print asterisks", not "render correctly".
func TestExcerptStripsMarkdownFromPlainText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			// Verbatim from the Houston Robotics Group feed. The heading is a
			// block boundary, so the excerpt is the line before it.
			name: "heading after a first line",
			in:   "Houston Robotics Group\n## **Houston Robotics Group**\n\nWe meet at TXRX.",
			want: "Houston Robotics Group",
		},
		{"bold", "Some **bold** text.", "Some bold text."},
		{"italic and underscores", "Some *italic* and __bold__ text.", "Some italic and bold text."},
		{"inline code", "Run `make build` first.", "Run make build first."},
		{"a link keeps its label", "See [our Discord](https://discord.gg/x) for details.", "See our Discord for details."},
		{"escaped punctuation", "select your city\\. and the career\\-forum", "select your city. and the career-forum"},
		{"a bullet marker", "- First item", "First item"},
		{"a blockquote marker", "> Quoted line", "Quoted line"},
		{
			// HTML descriptions keep their punctuation. An Ion description's
			// asterisks are content, and a blanket strip would eat them.
			name: "html is left alone",
			in:   "<p>Rated 5*, cost is $10 * 2 per head.</p>",
			want: "Rated 5*, cost is $10 * 2 per head.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := excerpt(tc.in); got != tc.want {
				t.Errorf("excerpt() = %q, want %q", got, tc.want)
			}
		})
	}
}
