package auth

import "testing"

func TestSafeNext(t *testing.T) {
	const home = "http://exp.lab/"
	cases := []struct{ in, want string }{
		// Allowed.
		{"http://focal.exp.lab/", "http://focal.exp.lab/"},
		{"http://ritual.exp.lab/tasks?filter=today#top", "http://ritual.exp.lab/tasks?filter=today#top"},
		{"http://exp.lab/", "http://exp.lab/"},
		{"https://git.exp.lab/kunchu47/platform", "https://git.exp.lab/kunchu47/platform"},
		{"http://FOCAL.EXP.LAB/x", "http://FOCAL.EXP.LAB/x"},
		{"http://a.b.exp.lab/", "http://a.b.exp.lab/"},
		{"/metrics", "http://exp.lab/metrics"},

		// Rejected: every one of these must fall back to the dashboard.
		{"", home},
		{"http://evil.com/", home},
		{"https://evil.com/?exp.lab", home},
		{"http://exp.lab.evil.com/", home},      // our domain as a prefix
		{"http://evilexp.lab/", home},           // missing dot before the suffix
		{"http://exp.lab@evil.com/", home},      // userinfo trick
		{"http://focal.exp.lab:8080/", home},    // unexpected port
		{"//evil.com/", home},                   // scheme-relative
		{"/\\evil.com", home},                   // backslash variant
		{"http:\\\\evil.com", home},
		{"javascript:alert(1)", home},
		{"data:text/html,hi", home},
		{"ftp://focal.exp.lab/", home},
		{"focal.exp.lab", home},                 // no scheme: ambiguous
		{"http://focal.exp.lab/\r\nSet-Cookie: x=1", home},
		{"\thttp://evil.com", home},
		{"http://", home},
	}
	for _, c := range cases {
		if got := SafeNext(c.in, "http", "exp.lab"); got != c.want {
			t.Errorf("SafeNext(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSafeNextRejectsOverlongInput(t *testing.T) {
	long := "http://focal.exp.lab/" + string(make([]byte, 3000))
	if got := SafeNext(long, "http", "exp.lab"); got != "http://exp.lab/" {
		t.Errorf("overlong next accepted: %d bytes", len(got))
	}
}
