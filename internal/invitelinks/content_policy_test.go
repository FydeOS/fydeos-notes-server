// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package invitelinks

import "testing"

func TestContainsURLOrDomain(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		// Plain domains and URLs.
		{"bare domain", "example.com", true},
		{"domain in sentence", "Visit example.com today", true},
		{"subdomain", "Join mail.example.co.uk now", true},
		{"hyphenated label", "my-team.io", true},
		{"digit label", "123.com", true},
		{"domain followed by punctuation", "go to example.org!", true},
		{"domain followed by full stop", "See example.net.", true},
		{"https url", "https://example.com/path?q=1", true},
		{"http url", "http://localhost:8080", true},
		{"ftp url", "ftp://files", true},
		{"other scheme", "Open telegram://join", true},
		{"www prefix", "www.example", true},
		{"uppercase", "EXAMPLE.COM", true},
		{"uppercase scheme", "HTTPS://EXAMPLE", true},
		{"mixed case", "ExAmPlE.CoM", true},
		{"email address", "contact a@b.com", true},
		{"long tld", "shop.photography", true},
		{"domain inside parentheses", "Team (example.com)", true},
		{"domain after CJK text", "访问example.com领奖", true},
		{"IDN label with ascii tld", "例子.com", true},

		// Unicode dot look-alikes.
		{"fullwidth full stop", "example．com", true},
		{"ideographic full stop", "example。com", true},
		{"halfwidth ideographic full stop", "example｡com", true},
		{"one dot leader", "example․com", true},
		{"middle dot", "example·com", true},
		{"bullet", "example•com", true},
		{"katakana middle dot", "example・com", true},
		{"bracketed dot", "example[.]com", true},
		{"bracketed dot word", "example(dot)com", true},
		{"fullwidth url", "ｈｔｔｐｓ：／／ｅｘａｍｐｌｅ", true},
		{"fullwidth domain", "ＥＸＡＭＰＬＥ．ＣＯＭ", true},

		// Invisible characters.
		{"zero-width space in label", "exa\u200bmple.com", true},
		{"zero-width space around dot", "example\u200b.\u200bcom", true},
		{"zero-width joiner", "example.c\u200do\u200dm", true},
		{"soft hyphen", "exam\u00adple.com", true},
		{"bidi override", "example.\u202ecom", true},
		{"BOM", "\ufeffexample.com", true},
		{"variation selector", "example.c\ufe0fom", true},
		{"word joiner in scheme", "ht\u2060tps://x", true},

		// Look-alike letters.
		{"cyrillic a and o", "exаmple.cоm", true},
		{"cyrillic tld", "example.\u0435\u0455", true},
		{"greek omicron", "gοοgle.com", true},
		{"mathematical bold", "𝐞𝐱𝐚𝐦𝐩𝐥𝐞.𝐜𝐨𝐦", true},
		{"accented letters", "exämplé.cöm", true},
		{"combining overlay", "e\u0336x\u0336a\u0336m\u0336p\u0336l\u0336e\u0336.\u0336c\u0336o\u0336m\u0336", true},

		// Enclosed alphanumerics.
		{"circled letters", "ⓔⓧⓐⓜⓟⓛⓔ.ⓒⓞⓜ", true},
		{"circled capitals", "ⒺⓍⒶⓂⓅⓁⒺ.ⒸⓄⓂ", true},
		{"parenthesised letters", "⒠⒳⒜⒨⒫⒧⒠.⒞⒪⒨", true},
		{"squared letters", "🄴🅇🄰🄼🄿🄻🄴.🄲🄾🄼", true},
		{"negative circled letters", "🅔🅧🅐🅜🅟🅛🅔.🅒🅞🅜", true},
		{"negative squared letters", "🅴🆇🅰🅼🅿🅻🅴.🅲🅾🅼", true},
		{"circled digits label", "①②③.com", true},
		{"regional indicators", "🇪🇽🇦🇲🇵🇱🇪.🇨🇴🇲", true},

		// Normal workspace names.
		{"empty", "", false},
		{"plain words", "Marketing Team", false},
		{"chinese", "产品设计团队", false},
		{"chinese with ideographic full stop", "我们的团队。欢迎加入", false},
		{"chinese sentence end", "研发中心。", false},
		{"japanese", "開発チーム・東京", false},
		{"japanese sentence", "プロジェクト管理。よろしく", false},
		{"korean", "디자인 팀", false},
		{"russian", "Команда разработки", false},
		{"greek", "Ομάδα σχεδιασμού", false},
		{"arabic", "فريق التصميم", false},
		{"emoji", "🚀 Launch Squad", false},
		{"flag emoji", "🇨🇳 China Team", false},
		{"version number", "Release 1.2.3", false},
		{"v version number", "App v2.10.4", false},
		{"decimal", "Budget 3.14 million", false},
		{"decimal with unit", "Plan 1.5kg", false},
		{"abbreviation e.g.", "Tools, e.g. hammers", false},
		{"abbreviation U.S.", "U.S. Office", false},
		{"abbreviation U.S.A.", "U.S.A. branch", false},
		{"abbreviation Ph.D.", "Ph.D. students", false},
		{"sentence end", "Welcome to the Team.", false},
		{"sentence then word", "Hello. World", false},
		{"ellipsis", "Wait...what", false},
		{"time", "Standup 10.30", false},
		{"ip-like numbers", "Room 192.168", false},
		{"underscore glued label", "my_team.com", false},
		{"tld glued to digits", "file.v2", false},
		{"tld glued to underscore", "notes.md_backup", false},
		{"single letter tld", "Plan a.b", false},
		{"slash without scheme", "R&D / QA", false},
		{"colon without slashes", "Team: Alpha", false},
		{"no letter before scheme separator", "odd ://", false},
		{"www alone", "www", false},
		{"invisible only", "\u200b\u200c\u200d", false},
		{"circled digits list", "① Planning ② Review", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsURLOrDomain(tt.text); got != tt.want {
				t.Fatalf("ContainsURLOrDomain(%q) = %v, want %v (normalized %q)", tt.text, got, tt.want, normalizeForLinkDetection(tt.text))
			}
			if got := containsURLOrDomain(tt.text); got != tt.want {
				t.Fatalf("containsURLOrDomain(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}
