package normalize

import (
	"regexp"
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

	paras, markup := paragraphs(description)

	// Meetup writes its ICS descriptions in Markdown and sends them as plain
	// text, so "## **Houston Robotics Group**" arrives verbatim and would be
	// published verbatim. Stripped only when the source carried no HTML tags
	// at all: an HTML description's asterisks and hashes are content, and a
	// blanket strip would eat them.
	if !markup {
		for i, para := range paras {
			paras[i] = stripMarkdown(para)
		}
	}

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
func paragraphs(s string) (out []string, markup bool) {
	var cur strings.Builder
	flush := func() {
		if text := strings.Join(strings.Fields(stripInvisible(cur.String())), " "); text != "" {
			out = append(out, text)
		}
		cur.Reset()
	}

	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			flush()
			return out, markup

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
				// A Markdown heading is a block boundary too, and Meetup uses
				// them. Without this the group name on the first line runs
				// into the heading below it.
				for j, line := range strings.Split(part, "\n") {
					if j > 0 && strings.HasPrefix(strings.TrimSpace(line), "#") {
						flush()
					}
					if j > 0 {
						cur.WriteString(" ")
					}
					cur.WriteString(line)
				}
			}

		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			markup = true
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

// stripInvisible removes zero-width characters.
//
// strings.Fields will not do it: unicode.IsSpace is false for U+200B and
// friends, because they are format characters rather than spaces. Ion's
// descriptions are full of them, pasted in from whatever wrote the copy, and
// they survive all the way into the published JSON as an invisible first
// character that makes an excerpt look like it begins with a space.
func stripInvisible(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\ufeff', '\u00ad':
			return -1
		}
		return r
	}, s)
}

// stripMarkdown removes the syntax Meetup sends inside a plain-text
// description, leaving the words.
//
// Deliberately not a Markdown parser. This runs on one paragraph that is about
// to be truncated to 220 characters, so the job is "do not print asterisks",
// not "render correctly". Anything it does not recognise is left alone, which
// is the same posture the ICS unescaper takes.
func stripMarkdown(s string) string {
	// Links first, so the label survives and the URL does not.
	s = markdownLink.ReplaceAllString(s, "$1")
	// Leading heading markers and blockquote markers.
	s = markdownLead.ReplaceAllString(s, "")
	// Emphasis. Bold before italic, so ** is not eaten one star at a time.
	for _, marker := range []string{"**", "__", "*", "_", "`"} {
		s = strings.ReplaceAll(s, marker, "")
	}
	// Meetup escapes punctuation the way Markdown does, and the ICS unescaper
	// correctly leaves those alone because they are not RFC 5545 escapes.
	s = markdownEscape.ReplaceAllString(s, "$1")
	return strings.Join(strings.Fields(s), " ")
}

var (
	markdownLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	markdownLead   = regexp.MustCompile(`(?m)^\s*(#{1,6}\s+|>\s+|[-*+]\s+)`)
	markdownEscape = regexp.MustCompile(`\\([\\` + "`" + `*_{}\[\]()#+\-.!])`)
)

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
