package i18n

import (
	"strings"
	"testing"
)

func TestTEnglish(t *testing.T) {
	if got := T(English, KeyHelp, nil); got == "" {
		t.Fatal("empty english help")
	}
}

func TestTChinese(t *testing.T) {
	got := T(Chinese, KeyStart, nil)
	if got == "" {
		t.Fatal("empty chinese start")
	}
	if got == T(English, KeyStart, nil) {
		t.Error("chinese start equals english start; not translated")
	}
}

func TestTPlaceholderName(t *testing.T) {
	got := T(English, KeyWelcomeNewMember, map[string]string{"name": "@alice"})
	if !strings.Contains(got, "@alice") {
		t.Errorf("expected @alice in %q", got)
	}
}

func TestTPlaceholderTZ(t *testing.T) {
	got := T(English, KeyTimezoneSet, map[string]string{"tz": "Asia/Shanghai"})
	if !strings.Contains(got, "Asia/Shanghai") {
		t.Errorf("expected timezone in %q", got)
	}
}

func TestTUnknownKeyReturnsEmpty(t *testing.T) {
	if got := T(Chinese, MessageKey("does-not-exist"), nil); got != "" {
		t.Errorf("expected empty for unknown key, got %q", got)
	}
}

func TestTFallbackToEnglishForUnsupportedLang(t *testing.T) {
	if got := T(Language("fr"), KeyHelp, nil); got == "" {
		t.Fatal("expected fallback to english")
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]Language{
		"en": English, "zh": Chinese, "ZH": Chinese, "zh-cn": Chinese,
		"chinese": Chinese, "中文": Chinese, "fr": English,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// MYC-54 #2: the member failure text must report that the result could not be
// confirmed, not claim that nothing was changed.
func TestMemberFailedTextReportsUnconfirmedResult(t *testing.T) {
	for _, lang := range []Language{English, Chinese} {
		got := T(lang, KeyMemberFailed, nil)
		if got == "" {
			t.Fatalf("%s: empty member failed text", lang)
		}
		for _, stale := range []string{"No change was made", "未做任何更改", "失败"} {
			if strings.Contains(got, stale) {
				t.Errorf("%s: member failed text %q must not contain %q", lang, got, stale)
			}
		}
	}
	if got := T(English, KeyMemberFailed, nil); !strings.Contains(got, "check member permissions") {
		t.Errorf("english member failed text %q must ask to check member permissions", got)
	}
	if got := T(Chinese, KeyMemberFailed, nil); !strings.Contains(got, "请检查成员权限") {
		t.Errorf("chinese member failed text %q must ask to check member permissions", got)
	}
}

// MYC-54 #3: the protected-target text must not bind to one action, so the
// same hint works for /restrict and /unrestrict.
func TestMemberProtectedTextIsActionAgnostic(t *testing.T) {
	for _, lang := range []Language{English, Chinese} {
		got := T(lang, KeyMemberProtected, nil)
		if got == "" {
			t.Fatalf("%s: empty member protected text", lang)
		}
		for _, bound := range []string{"restricted", "限制", "解除"} {
			if strings.Contains(got, bound) {
				t.Errorf("%s: member protected text %q must not bind to an action via %q", lang, got, bound)
			}
		}
	}
}

// MYC-54 #4: the usage hint must show examples for both commands.
func TestMemberUsageShowsBothCommands(t *testing.T) {
	for _, lang := range []Language{English, Chinese} {
		got := T(lang, KeyMemberUsage, nil)
		if got == "" {
			t.Fatalf("%s: empty member usage text", lang)
		}
		for _, want := range []string{"/restrict", "/unrestrict"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: member usage text %q must mention %q", lang, got, want)
			}
		}
	}
}

// MYC-55 SEC-1: the not-applicable text must call out banned/non-member users.
func TestMemberNotApplicableText(t *testing.T) {
	for _, lang := range []Language{English, Chinese} {
		got := T(lang, KeyMemberNotApplicable, nil)
		if got == "" {
			t.Fatalf("%s: empty member not-applicable text", lang)
		}
	}
	if got := T(English, KeyMemberNotApplicable, nil); !strings.Contains(got, "banned") {
		t.Errorf("english not-applicable text %q must mention banned users", got)
	}
	if got := T(Chinese, KeyMemberNotApplicable, nil); !strings.Contains(got, "封禁") {
		t.Errorf("chinese not-applicable text %q must mention banned users", got)
	}
}
