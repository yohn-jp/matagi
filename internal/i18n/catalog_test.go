package i18n

import (
	"bytes"
	"testing"
)

func TestResolvePrefersSavedLocaleAndSupportsHostTags(t *testing.T) {
	tests := []struct {
		saved string
		host  []string
		want  Locale
	}{
		{saved: "", want: English},
		{host: []string{"ja-JP"}, want: Japanese},
		{host: []string{"ja_JP.UTF-8"}, want: Japanese},
		{host: []string{"fr-FR", "ja"}, want: Japanese},
		{host: []string{"fr_FR.UTF-8", "C.UTF-8"}, want: English},
		{saved: "en", host: []string{"ja-JP"}, want: English},
		{saved: "ja", host: []string{"en-US"}, want: Japanese},
		{saved: "de", want: English},
		{saved: "JA", want: English},
	}
	for _, test := range tests {
		if got := Resolve(test.saved, test.host...); got != test.want {
			t.Errorf("Resolve(%q, %q) = %q, want %q", test.saved, test.host, got, test.want)
		}
	}
}

func TestLookupFallsBackToEnglishAndFormatsArguments(t *testing.T) {
	if got := Japanese.T("Start"); got != "開始" {
		t.Fatalf("Japanese Start = %q", got)
	}
	if got := English.T("Start"); got != "Start" {
		t.Fatalf("English Start = %q", got)
	}
	const missing = "a message without a Japanese entry"
	if Japanese.Has(missing) || Japanese.T(missing) != missing {
		t.Fatal("missing Japanese text did not fall back to English")
	}
	if got := Japanese.T("%d of %d slots", 3, 4); got != "3 of 4 slots" {
		t.Fatalf("formatted fallback = %q", got)
	}
	if got := Locale("xx").T("Start"); got != "Start" {
		t.Fatalf("unsupported locale = %q", got)
	}
}

func TestJapaneseCatalogCoversOperatorAndBrowserCopy(t *testing.T) {
	for _, message := range OperatorMessages {
		if !Japanese.Has(message) || Japanese.T(message) == message {
			t.Errorf("operator message %q has no Japanese translation", message)
		}
	}
	for _, message := range BrowserMessages {
		if !Japanese.Has(message) || Japanese.T(message) == message {
			t.Errorf("browser message %q has no Japanese translation", message)
		}
	}
	english := English.Table(BrowserMessages)
	japanese := Japanese.Table(BrowserMessages)
	for _, message := range BrowserMessages {
		if english[message] != message {
			t.Errorf("English table changed %q to %q", message, english[message])
		}
		if japanese[message] == message {
			t.Errorf("Japanese table did not translate %q", message)
		}
	}
}

func TestJapaneseFormatVerbsMatchEnglish(t *testing.T) {
	for english, japanese := range ja {
		if !bytes.Equal(formatVerbs(english), formatVerbs(japanese)) {
			t.Errorf("format verbs differ for %q: %q", english, japanese)
		}
	}
}

func TestSupportedLocalesAreExactlyEnglishAndJapanese(t *testing.T) {
	if len(Supported) != 2 || Supported[0] != English || Supported[1] != Japanese {
		t.Fatalf("Supported = %#v", Supported)
	}
	for _, value := range []string{"en", "ja"} {
		if !Valid(value) {
			t.Errorf("%q should be supported", value)
		}
	}
	for _, value := range []string{"", "en-US", "fr", "JA"} {
		if Valid(value) {
			t.Errorf("%q should not be selectable", value)
		}
	}
}
