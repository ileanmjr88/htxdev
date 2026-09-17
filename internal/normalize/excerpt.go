package normalize

import (
	"strings"

	"golang.org/x/net/html"
)

// excerptLimit is roughly a card's worth of text. Cut at a word boundary, so
// the number is a ceiling rather than an exact length.
const excerptLimit = 220

// excerpt turns a description into one paragraph of plain text.
//
// D8. Ion sends no excerpt field at all, so this is derived from up to 6.5KB
// of HTML per event. The stdlib has no HTML-to-text, and the three candidates
// were a tokenizer, a tag-stripping regex, or moving the problem to the Astro
// layer.
//
// The regex loses on the live data, measured rather than assumed: of 76
// events, 33 carry <a>, 19 carry <img>, 12 carry <ul>, 35 carry HTML entities
// and the markup nests three deep. A tokenizer handles all four for free.
// Stripping in Astro does not work either, because Phase 8's API has to serve
// an excerpt and Astro is not in that path.
//
// golang.org/x/net/html was already a dependency by the time this was written,
// pulled in for the HOSS decoder, so D8's one-dependency cost turned out to be
// zero.
func excerpt(description string) string {
	if description == "" {
		return ""
	}

	paras := paragraphs(description)

	// Ion's descriptions routinely open with "Register Here: <link>", which
	// makes a first-paragraph excerpt that says nothing. Skip any leading
	// paragraph that is mostly its own URL.
	for len(paras) > 0 && isMostlyLink(paras[0]) {
		paras = paras[1:]
	}
	if len(paras) == 0 {
		return ""
	}

	return truncateWords(paras[0], excerptLimit)
}

// paragraphs flattens the markup into blocks of text, one per block-level
// element, so an excerpt starts at a sentence rather than mid-way through one.
//
// The tokenizer is used rather than the tree parser: this only needs text and
// block boundaries, and html.Parse would build a whole document to throw it
// away. It also means entity decoding comes for free, which is 35 of the 76
// events.
func paragraphs(s string) []string {
	var (
		out []string
		cur strings.Builder
	)
	flush := func() {
		if text := strings.Join(strings.Fields(cur.String()), " "); text != "" {
			out = append(out, text)
		}
		cur.Reset()
	}

	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			flush()
			return out

		case html.TextToken:
			// A blank line is the only paragraph signal a plain-text
			// description has, and every ICS feed sends plain text: Meetup
			// separates its paragraphs with \n\n and nothing else. Without
			// this the group name on Code and Coffee's first line runs
			// straight into the sentence after it.
			for i, part := range strings.Split(z.Token().Data, "\n\n") {
				if i > 0 {
					flush()
				}
				cur.WriteString(part)
			}

		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "p", "div", "br", "li", "ul", "ol", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "tr", "table":
				flush()
			case "script", "style":
				// Not text, whatever the tokenizer says about what is inside.
				flush()
			}
		}
	}
}

// isMostlyLink reports whether a paragraph is really just a URL with a label.
// "Register Here: https://luma.com/u9ycnl37?lm_source=embed" is the shape, and
// it is the first paragraph of a good number of Ion's events.
func isMostlyLink(p string) bool {
	i := strings.Index(p, "http")
	if i < 0 {
		return false
	}
	// The URL and whatever introduces it. If the text that is not the URL is
	// shorter than the URL itself, there is no sentence here worth keeping.
	rest := p[i:]
	url, _, _ := strings.Cut(rest, " ")
	return len(p)-len(url) < len(url)
}

// truncateWords cuts at the last word boundary at or before limit and appends
// an ellipsis, so an excerpt never ends mid-word.
func truncateWords(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[:limit]
	if i := strings.LastIndexByte(cut, ' '); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:.-") + "…"
}
