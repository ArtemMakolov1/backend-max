package contentdedup

import "testing"

func TestCanonicalSourceURLPreservesArticleIdentityAndDropsOnlyTracking(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"http://WWW.Example.com:80/article/?utm_source=max&ysclid=123&id=42#part", "https://example.com/article?id=42"},
		{"https://example.com/article?b=2&a=1&UTM_campaign=post", "https://example.com/article?a=1&b=2"},
		{"https://example.com/article?id=43", "https://example.com/article?id=43"},
		{"https://EXAMPLE.com:443/article?ref=semantic&source=42&fbclid=tracking", "https://example.com/article?ref=semantic&source=42"},
		{"https://example.com:8443/article?start=20&utm_medium=social", "https://example.com:8443/article?start=20"},
		{"https://example.com/a%2Fb/", "https://example.com/a%2Fb"},
		{"https://example.com/a/b/", "https://example.com/a/b"},
		{"https://user:secret@example.com/article", ""},
		{"max://user/123", ""},
		{"not a URL", ""},
	} {
		if got := CanonicalSourceURL(test.input); got != test.want {
			t.Errorf("canonical source %q = %q, want %q", test.input, got, test.want)
		}
	}
}
