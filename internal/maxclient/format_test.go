package maxclient

import "testing"

func TestNormalizeMessageTextFlattensOnlySupportedMarkdownHeadings(t *testing.T) {
	t.Parallel()
	input := "## Привет\r\n" +
		"++Меня зовут Фома++\n" +
		"### Третий\n#### Четвёртый\n##### Пятый\n###### Шестой\n" +
		"# Уже поддерживается\n" +
		"##Без пробела\n####### Семь решёток\n" +
		"```markdown\n## В блоке кода\n```\n" +
		"~~~\n### Тоже код\n~~~\n" +
		"## После кода"
	want := "# Привет\r\n" +
		"++Меня зовут Фома++\n" +
		"# Третий\n# Четвёртый\n# Пятый\n# Шестой\n" +
		"# Уже поддерживается\n" +
		"##Без пробела\n####### Семь решёток\n" +
		"```markdown\n## В блоке кода\n```\n" +
		"~~~\n### Тоже код\n~~~\n" +
		"# После кода"

	if got := normalizeMessageText(input, FormatMarkdown); got != want {
		t.Fatalf("normalizeMessageText() =\n%q\nwant\n%q", got, want)
	}
}

func TestNormalizeMessageTextLeavesHTMLAndNonHeadingsUntouched(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		text   string
		format Format
	}{
		"html":           {text: "## Не Markdown\n<h2>HTML остаётся HTML</h2>", format: FormatHTML},
		"plain":          {text: "## Не форматировать без format", format: ""},
		"escaped":        {text: `\## Экранированный заголовок`, format: FormatMarkdown},
		"blockquote":     {text: "> ## Заголовок в цитате", format: FormatMarkdown},
		"indented":       {text: "  ## Заголовок с отступом", format: FormatMarkdown},
		"unclosed fence": {text: "```\n## Остаётся кодом", format: FormatMarkdown},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeMessageText(test.text, test.format); got != test.text {
				t.Fatalf("normalizeMessageText(%q, %q) = %q", test.text, test.format, got)
			}
		})
	}
}
