package maxclient

import "testing"

func TestSafeAssetURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		url  string
		want string
	}{
		{name: "MAX CDN", url: "https://cdn.max.ru/channels/icon.png", want: "https://cdn.max.ru/channels/icon.png"},
		{name: "OneMe CDN with signed query", url: "https://i.oneme.ru/icon.webp?size=256&sig=ok", want: "https://i.oneme.ru/icon.webp?size=256&sig=ok"},
		{name: "trimmed", url: "  https://max.ru/assets/icon.png  ", want: "https://max.ru/assets/icon.png"},
		{name: "HTTP", url: "http://cdn.max.ru/icon.png"},
		{name: "lookalike host", url: "https://cdn.max.ru.evil.example/icon.png"},
		{name: "suffix lookalike", url: "https://evilmax.ru/icon.png"},
		{name: "userinfo", url: "https://user@cdn.max.ru/icon.png"},
		{name: "custom port", url: "https://cdn.max.ru:8443/icon.png"},
		{name: "opaque URL", url: "https:cdn.max.ru/icon.png"},
		{name: "fragment", url: "https://cdn.max.ru/icon.png#track"},
		{name: "control character", url: "https://cdn.max.ru/icon.png\nhttps://evil.example"},
		{name: "host only", url: "https://cdn.max.ru"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SafeAssetURL(tt.url); got != tt.want {
				t.Fatalf("SafeAssetURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}
