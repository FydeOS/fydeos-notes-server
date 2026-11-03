// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// ContainsURLOrDomain reports whether text contains something that a mail
// client or a human reader could follow as a link: a URL or a bare domain
// name, including visually obfuscated spellings. Workspace names are shown in
// invitation emails, so names that fail this check must not be mailed.
//
// The text is first reduced to a comparison form:
//
//   - invisible code points are removed (format characters such as zero-width
//     spaces/joiners, bidi controls, soft hyphen and BOM; variation selectors;
//     other default-ignorable code points; non-whitespace control characters);
//   - enclosed, circled, parenthesised and squared alphanumerics (ⓐ, ⒜, 🄰, 🅐,
//     ①, ❶, regional indicators) become their plain letters and digits;
//   - the text is NFKC-normalised (full-width and mathematical letters,
//     full-width ":" and "/" and so on);
//   - dot-like punctuation (。 ． ｡ ․ ﹒ · • ∙ ⋅ ・ and similar) becomes ".";
//   - every remaining non-ASCII rune whose Unicode confusable skeleton is
//     plain ASCII (for example Cyrillic "а" or Greek "ο") is replaced by that
//     skeleton; ASCII itself is never remapped, so "1.2" stays a number;
//   - combining marks are dropped after canonical decomposition and the text
//     is lower-cased;
//   - bracketed dots such as "[.]", "(.)", "[dot]" become ".".
//
// The comparison form then matches when it contains:
//
//   - a scheme separator "://" directly after a scheme name (http://,
//     https://, ftp://, ...);
//   - a token starting with "www." followed by a label character;
//   - a domain-like pair "label.tld": label is a non-empty run of letters,
//     digits and hyphens (not starting or ending with a hyphen), and tld is
//     2–63 ASCII letters. The pair must not be glued to other word characters:
//     letters, digits, "_" and "-" on either side extend the token, and an
//     extended label or tld no longer qualifies. Han, kana and Hangul text
//     directly after the tld does not glue, because those scripts are written
//     without spaces and link detectors stop there.
//
// Everything that is not a domain-like pair stays allowed, so version numbers
// (1.2.3), decimals (3.14), abbreviations with single-letter parts (e.g.,
// U.S.) and sentence punctuation ("Team." or "Team. Alpha") do not match. An
// email address matches because of its domain part.
func ContainsURLOrDomain(text string) bool {
	return containsURLOrDomain(text)
}

func containsURLOrDomain(text string) bool {
	normalized := normalizeForLinkDetection(text)
	return containsSchemeURL(normalized) || containsDomainToken(normalized)
}

// normalizeForLinkDetection produces the comparison form described on
// ContainsURLOrDomain.
func normalizeForLinkDetection(text string) string {
	var pre strings.Builder
	pre.Grow(len(text))
	for _, r := range text {
		if isInvisible(r) {
			continue
		}
		if mapped, ok := enclosedAlphanumeric(r); ok {
			pre.WriteString(mapped)
			continue
		}
		pre.WriteRune(r)
	}

	var mapped strings.Builder
	mapped.Grow(pre.Len())
	for _, r := range norm.NFKC.String(pre.String()) {
		switch {
		case isInvisible(r):
		case r < utf8.RuneSelf:
			mapped.WriteRune(r)
		case isDotLike(r):
			mapped.WriteByte('.')
		default:
			if skeleton, ok := asciiSkeleton(r); ok {
				mapped.WriteString(skeleton)
			} else {
				mapped.WriteRune(r)
			}
		}
	}

	var out strings.Builder
	out.Grow(mapped.Len())
	for _, r := range norm.NFD.String(mapped.String()) {
		switch {
		case unicode.In(r, unicode.Mn, unicode.Me), isInvisible(r):
		case isDotLike(r):
			out.WriteByte('.')
		default:
			out.WriteRune(unicode.ToLower(r))
		}
	}
	return bracketedDots.Replace(out.String())
}

var bracketedDots = strings.NewReplacer(
	"[.]", ".", "(.)", ".", "{.}", ".", "<.>", ".",
	"[dot]", ".", "(dot)", ".", "{dot}", ".", "<dot>", ".",
)

// isInvisible reports code points that render as nothing (or nearly nothing)
// and can be slipped between the characters of a domain name.
func isInvisible(r rune) bool {
	if unicode.IsControl(r) {
		return !unicode.IsSpace(r)
	}
	return unicode.In(r,
		unicode.Cf,
		unicode.Variation_Selector,
		unicode.Other_Default_Ignorable_Code_Point,
	)
}

func isDotLike(r rune) bool {
	switch r {
	case '.',
		0x00B7,  // middle dot
		0x0387,  // Greek ano teleia
		0x0701,  // Syriac supralinear full stop
		0x0702,  // Syriac sublinear full stop
		0x16EB,  // runic single punctuation
		0x2022,  // bullet
		0x2024,  // one dot leader
		0x2027,  // hyphenation point
		0x2219,  // bullet operator
		0x22C5,  // dot operator
		0x2981,  // Z notation spot
		0x2E31,  // word separator middle dot
		0x2E3C,  // stenographic full stop
		0x3002,  // ideographic full stop
		0x30FB,  // katakana middle dot
		0xA4F8,  // Lisu letter tone mya ti
		0xA60E,  // Vai full stop
		0xFE52,  // small full stop
		0xFF0E,  // fullwidth full stop
		0xFF61,  // halfwidth ideographic full stop
		0xFF65,  // halfwidth katakana middle dot
		0x10A50: // Kharoshthi punctuation dot
		return true
	}
	return false
}

// enclosedAlphanumeric maps enclosed letters and digits whose NFKC form is
// not the bare character (parenthesised forms become "(a)", negative circled
// and dingbat forms have no decomposition at all).
func enclosedAlphanumeric(r rune) (string, bool) {
	letter := func(base rune) (string, bool) { return string('a' + r - base), true }
	number := func(base rune, first int) (string, bool) { return itoa(first + int(r-base)), true }
	switch {
	case r >= 0x2474 && r <= 0x2487: // ⑴..⒇
		return number(0x2474, 1)
	case r >= 0x249C && r <= 0x24B5: // ⒜..⒵
		return letter(0x249C)
	case r >= 0x24B6 && r <= 0x24CF: // Ⓐ..Ⓩ
		return letter(0x24B6)
	case r >= 0x24D0 && r <= 0x24E9: // ⓐ..ⓩ
		return letter(0x24D0)
	case r == 0x24EA, r == 0x24FF, r == 0x1F10B, r == 0x1F10C: // ⓪ ⓿ 🄋 🄌
		return "0", true
	case r >= 0x24EB && r <= 0x24F4: // ⓫..⓴
		return number(0x24EB, 11)
	case r >= 0x24F5 && r <= 0x24FE: // ⓵..⓾
		return number(0x24F5, 1)
	case r >= 0x2776 && r <= 0x277F: // ❶..❿
		return number(0x2776, 1)
	case r >= 0x2780 && r <= 0x2789: // ➀..➉
		return number(0x2780, 1)
	case r >= 0x278A && r <= 0x2793: // ➊..➓
		return number(0x278A, 1)
	case r >= 0x1F110 && r <= 0x1F129: // 🄐..🄩 parenthesised
		return letter(0x1F110)
	case r == 0x1F12A: // 🄪 tortoise-shell bracketed S
		return "s", true
	case r == 0x1F12B: // 🄫
		return "c", true
	case r == 0x1F12C: // 🄬
		return "r", true
	case r == 0x1F12D: // 🄭
		return "cd", true
	case r == 0x1F12E: // 🄮
		return "wz", true
	case r >= 0x1F130 && r <= 0x1F149: // 🄰..🅉 squared
		return letter(0x1F130)
	case r >= 0x1F150 && r <= 0x1F169: // 🅐..🅩 negative circled
		return letter(0x1F150)
	case r >= 0x1F170 && r <= 0x1F189: // 🅰..🆉 negative squared
		return letter(0x1F170)
	case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicators
		return letter(0x1F1E6)
	}
	return "", false
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// asciiSkeleton returns the confusable skeleton of a non-ASCII rune when that
// skeleton, without combining marks, is non-empty ASCII. Skeletons that stay
// non-ASCII are ignored so that ordinary text in other scripts is kept as is.
func asciiSkeleton(r rune) (string, bool) {
	skeleton, ok := unicodeSkeletonMappings[r]
	if !ok {
		return "", false
	}
	var b strings.Builder
	for _, sr := range norm.NFD.String(skeleton) {
		if unicode.In(sr, unicode.Mn, unicode.Me) {
			continue
		}
		if sr >= utf8.RuneSelf {
			return "", false
		}
		b.WriteRune(sr)
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

func containsSchemeURL(s string) bool {
	for offset := 0; ; {
		i := strings.Index(s[offset:], "://")
		if i < 0 {
			return false
		}
		i += offset
		if i > 0 && isASCIILetter(rune(s[i-1])) {
			return true
		}
		offset = i + 3
	}
}

func containsDomainToken(s string) bool {
	runs := strings.FieldsFunc(s, func(r rune) bool { return !isTokenRune(r) })
	for _, run := range runs {
		if strings.HasPrefix(run, "www.") && len(run) > len("www.") && run[len("www.")] != '.' {
			return true
		}
		labels := strings.Split(run, ".")
		for i := 1; i < len(labels); i++ {
			if isHostLabel(labels[i-1]) && isTopLevelLabel(labels[i]) {
				return true
			}
		}
	}
	return false
}

// isTokenRune reports runes that belong to a host-name-like token: word
// characters (which glue), hyphens and dots.
func isTokenRune(r rune) bool {
	return r == '.' || r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mc, r)
}

func isHostLabel(label string) bool {
	if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	return !strings.ContainsRune(label, '_')
}

func isTopLevelLabel(label string) bool {
	n := 0
	for n < len(label) && isASCIILetter(rune(label[n])) {
		n++
	}
	if n < 2 || n > 63 {
		return false
	}
	if n == len(label) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(label[n:])
	return unicode.In(next, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func isASCIILetter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}
