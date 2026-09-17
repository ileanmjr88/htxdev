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
