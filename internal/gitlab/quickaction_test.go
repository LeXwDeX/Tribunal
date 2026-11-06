package gitlab

import "testing"

func TestNeutralizeQuickActions(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "/close", "&#47;close"},
		{"indented", "text\n  /merge now\n\t/approve", "text\n  &#47;merge now\n\t&#47;approve"},
		{"crlf", "a\r\n/close\r\nb", "a\r\n&#47;close\r\nb"},
		{"lone cr", "a\r/close", "a\r&#47;close"},
		{"backtick fence", "```\n/close\n```\n/close", "```\n/close\n```\n&#47;close"},
		{"tilde fence", "~~~\n/close\n~~~", "~~~\n/close\n~~~"},
		{"indented fence", "  ```go\n/close\n  ```", "  ```go\n/close\n  ```"},
		{"longer fence needs longer close", "````\n```\n/close\n````\n/x", "````\n```\n/close\n````\n&#47;x"},
		{"unclosed fence is not trusted", "```\n/close", "```\n&#47;close"},
		{"four space indent is not a fence", "    ```\n/close\n    ```", "    ```\n&#47;close\n    ```"},
		{"inline code", "`/close` and a/b", "`/close` and a/b"},
		{"blockquote", "> /close", "> /close"},
		{"list item", "- /close", "- /close"},
		{"zero width prefix", "\u200b/close", "\u200b/close"},
		{"already escaped", "&#47;close", "&#47;close"},
		{"url", "see https://x/y\n//comment", "see https://x/y\n&#47;/comment"},
		{"no slash", "hello", "hello"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NeutralizeQuickActions(tc.in)
			if got != tc.want {
				t.Fatalf("NeutralizeQuickActions(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := NeutralizeQuickActions(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}
