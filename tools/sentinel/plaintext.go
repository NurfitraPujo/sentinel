package main

import (
	"regexp"
	"strings"
)

// plainMarkdown turns a Markdown comment excerpt or progress note into plain text for a one-line table cell: heading
// marks, blockquote marks, unordered-list bullets, task boxes, code fences and rules go; emphasis, strikethrough and
// inline-code backticks are dropped around their text; a link or image becomes its text. Ordered-list numbers stay,
// because the order is content. Inline code is left verbatim inside (`a_b_c` keeps its underscores). The server cuts an
// excerpt at a fixed length, so an unterminated `**` or backtick at the end is dropped too. It does not collapse
// whitespace; oneLine does. Only the table uses this -- `--json` prints the body exactly as the API sent it.
func plainMarkdown(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if mdFence.MatchString(t) || mdRule.MatchString(t) {
			continue
		}
		for {
			n := mdBlockPrefix.ReplaceAllString(t, "")
			if n == t {
				break
			}
			t = n
		}
		out = append(out, mdInline(t))
	}
	return strings.Join(out, "\n")
}

var (
	mdFence = regexp.MustCompile("^(```|~~~)")
	mdRule  = regexp.MustCompile(`^([-*_])(\s*[-*_]){2,}$`)
	// One leading block marker: a heading's #s, a blockquote's >, an unordered bullet, a task box.
	mdBlockPrefix = regexp.MustCompile(`^(#{1,6}(\s+|$)|>\s?|[-*+]\s+|\[[ xX]\]\s+)`)

	mdCodeSpan = regexp.MustCompile("`+([^`]*)`+")
	mdImage    = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	mdRefLink  = regexp.MustCompile(`\[([^\]]+)\]\[[^\]]*\]`)
	mdAutoLink = regexp.MustCompile(`<((?:https?|mailto):[^>\s]+)>`)
	mdStrong   = regexp.MustCompile(`\*\*(\S(?:.*?\S)?)\*\*`)
	mdStrike   = regexp.MustCompile(`~~(\S(?:.*?\S)?)~~`)
	mdEmStar   = regexp.MustCompile(`\*(\S(?:[^*]*?\S)?)\*`)
	// `__x__` and `_x_` only at word edges, so snake_case and __init__-like names survive.
	mdStrongUnder = regexp.MustCompile(`(^|[^\pL\pN_])__(\S(?:.*?\S)?)__($|[^\pL\pN_])`)
	mdEmUnder     = regexp.MustCompile(`(^|[^\pL\pN_])_(\S(?:[^_]*?\S)?)_($|[^\pL\pN_])`)
	mdStray       = regexp.MustCompile("\\*\\*|~~|`")
)

// mdInline strips inline syntax from one line, leaving code spans' contents untouched.
func mdInline(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range mdCodeSpan.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(mdInlineText(s[last:m[0]]))
		b.WriteString(s[m[2]:m[3]])
		last = m[1]
	}
	b.WriteString(mdInlineText(s[last:]))
	return b.String()
}

func mdInlineText(s string) string {
	s = mdImage.ReplaceAllString(s, "$1")
	s = mdLink.ReplaceAllString(s, "$1")
	s = mdRefLink.ReplaceAllString(s, "$1")
	s = mdAutoLink.ReplaceAllString(s, "$1")
	s = mdStrong.ReplaceAllString(s, "$1")
	s = mdStrike.ReplaceAllString(s, "$1")
	s = mdEmStar.ReplaceAllString(s, "$1")
	for _, re := range []*regexp.Regexp{mdStrongUnder, mdEmUnder} {
		// Adjacent matches share an edge character, so repeat until nothing changes.
		for {
			n := re.ReplaceAllString(s, "$1$2$3")
			if n == s {
				break
			}
			s = n
		}
	}
	return mdStray.ReplaceAllString(s, "")
}
