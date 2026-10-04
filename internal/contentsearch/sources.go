package contentsearch

import (
	"net"
	"net/url"
	"strings"
	"time"
)

type exaRequest struct {
	Query    string      `json:"query"`
	Type     string      `json:"type"`
	Contents exaContents `json:"contents"`
}

type exaContents struct {
	Highlights bool              `json:"highlights"`
	Extras     *exaExtrasRequest `json:"extras,omitempty"`
}

type exaExtrasRequest struct {
	ImageLinks int `json:"imageLinks"`
}

type exaResponse struct {
	Results *[]exaSource `json:"results"`
}

type exaSource struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	PublishedDate string   `json:"publishedDate"`
	Highlights    []string `json:"highlights"`
	Image         string   `json:"image"`
	Extras        struct {
		ImageLinks     []string `json:"imageLinks"`
		RichImageLinks []struct {
			URL string `json:"url"`
			Alt string `json:"alt"`
		} `json:"richImageLinks"`
	} `json:"extras"`
}

type tavilyRequest struct {
	Query                    string `json:"query"`
	SearchDepth              string `json:"search_depth"`
	MaxResults               int    `json:"max_results"`
	Language                 string `json:"language"`
	IncludeImages            bool   `json:"include_images,omitempty"`
	IncludeImageDescriptions bool   `json:"include_image_descriptions,omitempty"`
}

type tavilyResponse struct {
	Results *[]tavilySource `json:"results"`
	// Top-level images are intentionally ignored: they do not establish which
	// result page owns an image. Never attach them to an arbitrary source.
}

type tavilySource struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Content       string `json:"content"`
	RawContent    string `json:"raw_content"`
	PublishedDate string `json:"published_date"`
	Images        []struct {
		URL         string `json:"url"`
		Description string `json:"description"`
	} `json:"images"`
}

func (c *Client) exaSources(input []exaSource, media bool) []Source {
	sources := make([]Source, 0, min(len(input), maxSources))
	for _, item := range input {
		content := strings.Join(item.Highlights, "\n\n")
		source := Source{Title: c.text(item.Title, 250), URL: c.safeURL(item.URL), Content: c.text(content, maxContentRunes), PublishedAt: normalizeDate(item.PublishedDate)}
		if source.URL == "" {
			continue
		}
		if media {
			for _, image := range item.Extras.RichImageLinks {
				source.Images = c.appendImage(source.Images, Image{URL: image.URL, Description: image.Alt})
			}
			for _, image := range item.Extras.ImageLinks {
				source.Images = c.appendImage(source.Images, Image{URL: image})
			}
		}
		// Metadata's representative image is available without extra extraction
		// and remains useful for article/video cards. It is only a preview.
		source.Images = c.appendImage(source.Images, Image{URL: item.Image, PreviewOnly: true})
		if source.Title == "" || (source.Content == "" && len(source.Images) == 0) {
			continue
		}
		sources = appendSource(sources, source)
	}
	return sources
}

func (c *Client) tavilySources(input []tavilySource, media bool) []Source {
	sources := make([]Source, 0, min(len(input), maxSources))
	for _, item := range input {
		content := item.Content
		if strings.TrimSpace(content) == "" {
			content = item.RawContent
		}
		source := Source{Title: c.text(item.Title, 250), URL: c.safeURL(item.URL), Content: c.text(content, maxContentRunes), PublishedAt: normalizeDate(item.PublishedDate)}
		if source.URL == "" {
			continue
		}
		if media {
			for _, image := range item.Images {
				source.Images = c.appendImage(source.Images, Image{URL: image.URL, Description: image.Description})
			}
		}
		if source.Title == "" || (source.Content == "" && len(source.Images) == 0) {
			continue
		}
		sources = appendSource(sources, source)
	}
	return sources
}

func usableSources(sources []Source, kind string) []Source {
	usable := make([]Source, 0, len(sources))
	for _, source := range sources {
		// A text article about making memes cannot substitute for an actual
		// found image. Check this after DNS so unsafe previews trigger fallback.
		if kind == "meme" && len(source.Images) == 0 {
			continue
		}
		if source.Content != "" || len(source.Images) > 0 {
			usable = append(usable, source)
		}
	}
	return usable
}

func appendSource(sources []Source, source Source) []Source {
	for i := range sources {
		if sources[i].URL != source.URL {
			continue
		}
		if sources[i].Content == "" {
			sources[i].Content = source.Content
		}
		if sources[i].PublishedAt == "" {
			sources[i].PublishedAt = source.PublishedAt
		}
		for _, image := range source.Images {
			found := false
			for j := range sources[i].Images {
				if sources[i].Images[j].URL == image.URL {
					if sources[i].Images[j].PreviewOnly && !image.PreviewOnly {
						sources[i].Images[j] = image
					}
					found = true
					break
				}
			}
			if !found && len(sources[i].Images) < maxSourceImages {
				sources[i].Images = append(sources[i].Images, image)
			}
		}
		return sources
	}
	if len(sources) < maxSources {
		return append(sources, source)
	}
	return sources
}

func (c *Client) appendImage(images []Image, image Image) []Image {
	image.URL = c.safeURL(image.URL)
	if image.URL == "" {
		return images
	}
	image.Description = c.text(image.Description, 600)
	for i := range images {
		if images[i].URL == image.URL {
			if images[i].PreviewOnly && !image.PreviewOnly {
				images[i] = image
			}
			return images
		}
	}
	if len(images) < maxSourceImages {
		return append(images, image)
	}
	return images
}

func normalizeDate(value string) string {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC1123, time.RFC1123Z, "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// safeURL is a lexical source validation step, not a claim about DNS or download
// access. Mediafetch later validates all resolved IPs and pins the connection.
func (c *Client) safeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\t\\") {
		return ""
	}
	for _, key := range []string{c.exaKey, c.tavilyKey} {
		if key != "" && (strings.Contains(raw, key) || strings.Contains(raw, url.QueryEscape(key))) {
			return ""
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") {
		return ""
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return ""
	}
	for _, key := range []string{c.exaKey, c.tavilyKey} {
		if key != "" && strings.Contains(decoded, key) {
			return ""
		}
	}
	host := strings.ToLower(u.Hostname())
	if !publicHostname(host) {
		return ""
	}
	// Fragment-only differences must not generate duplicate source cards.
	u.Fragment, u.RawFragment = "", ""
	u.Host = host
	return u.String()
}

func publicHostname(host string) bool {
	if len(host) > 253 || strings.HasSuffix(host, ".") || net.ParseIP(host) != nil {
		return false
	}
	for _, suffix := range []string{".local", ".localhost", ".internal", ".lan", ".home.arpa", ".test", ".invalid", ".example", ".onion"} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	for _, r := range labels[len(labels)-1] {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return len(labels[len(labels)-1]) >= 2
}
