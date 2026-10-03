package slackbridge

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/agentbus"
)

func TestWithMentions(t *testing.T) {
	ids := map[string]string{"alex": "UALEX", "jane.d": "UJANE"}
	cases := map[string]string{
		"@alex done":                 "<@UALEX> done",
		"ping @Alex.":                "ping <@UALEX>.",
		"(@jane.d) see":              "(<@UJANE>) see",
		"mail bob@alex.com":          "mail bob@alex.com",
		"@nobody hi":                 "@nobody hi",
		"<!channel> & <@UEVE> a<b>c": "&lt;!channel&gt; &amp; &lt;@UEVE&gt; a&lt;b&gt;c",
	}
	for in, want := range cases {
		if got := withMentions(in, ids); got != want {
			t.Errorf("withMentions(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlainText(t *testing.T) {
	labels := map[string]string{"UALEX": "alex"}
	got := plainText("<@UALEX> and <@UEVE|eve>: see <https://x.test/a?b=1&amp;c=2|link> &lt;tag&gt; &amp;", labels)
	want := "@alex and @UEVE: see https://x.test/a?b=1&c=2 <tag> &"
	if got != want {
		t.Fatalf("plainText = %q, want %q", got, want)
	}
}

func TestParseAddressed(t *testing.T) {
	ok := map[string][2]string{
		"flyer: do X":                   {"flyer", "do X"},
		"pc/comms-3a9e9c:  rebase\nnow": {"pc/comms-3a9e9c", "rebase\nnow"},
	}
	for in, want := range ok {
		target, body, found := parseAddressed(in)
		if !found || target != want[0] || body != want[1] {
			t.Errorf("parseAddressed(%q) = %q, %q, %v", in, target, body, found)
		}
	}
	for _, in := range []string{"https://example.com", "hello there", "flyer:", ": x", "no colon"} {
		if _, _, found := parseAddressed(in); found {
			t.Errorf("parseAddressed(%q) matched", in)
		}
	}
}

func TestParseAtTagged(t *testing.T) {
	cases := []struct {
		raw, text, name, body string
		ok                    bool
	}{
		{"@flyer2 look here", "@flyer2 look here", "flyer2", "look here", true},
		{"  @flyer2: look", "@flyer2: look", "flyer2", "look", true},
		{"@pc/work-aaaa1111 hi\nthere", "@pc/work-aaaa1111 hi\nthere", "pc/work-aaaa1111", "hi\nthere", true},
		{"@flyer2", "@flyer2", "", "", false},
		{"@flyer2   ", "@flyer2   ", "", "", false},
		{"@flyer2:look", "@flyer2:look", "", "", false},
		{"<@UALEX> look", "@alex look", "", "", false},
		{"hi @flyer2 look", "hi @flyer2 look", "", "", false},
		{"@ look", "@ look", "", "", false},
	}
	for _, c := range cases {
		name, body, ok := parseAtTagged(c.raw, c.text)
		if name != c.name || body != c.body || ok != c.ok {
			t.Errorf("parseAtTagged(%q) = %q, %q, %v", c.raw, name, body, ok)
		}
	}
}

func TestParseCommand(t *testing.T) {
	cases := []struct {
		in, verb, user string
		isCmd          bool
	}{
		{"<@UBOT> allow <@UJANE>", "allow", "UJANE", true},
		{"<@UBOT>  Remove  <@UJANE|jane>", "remove", "UJANE", true},
		{"<@UBOT> hello", "", "", true},
		{"<@UOTHER> allow <@UJANE>", "", "", false},
		{"flyer: <@UBOT> allow <@UJANE>", "", "", false},
	}
	for _, c := range cases {
		verb, user, isCmd := parseCommand(c.in, "UBOT")
		if verb != c.verb || user != c.user || isCmd != c.isCmd {
			t.Errorf("parseCommand(%q) = %q %q %v", c.in, verb, user, isCmd)
		}
	}
}

func TestLabels(t *testing.T) {
	cases := map[string]string{"Jane D": "jane-d", "  Ñ!! ": "user", "alex.smith.": "alex.smith", "Bob_O'Neil": "bob_o-neil"}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if got := emailLabel("Alex.Smith@example.com"); got != "alex.smith" {
		t.Fatalf("emailLabel = %q", got)
	}
}

func TestSessionHeader(t *testing.T) {
	got := sessionHeader(agentbus.Outbound{Name: "flyer", Address: "pc/flyer-aaaaaa", Machine: "pc"})
	want := "*flyer* · pc/flyer-aaaaaa · pc"
	if got != want {
		t.Fatalf("header = %q, want %q", got, want)
	}
	if got = sessionHeader(agentbus.Outbound{Address: "pc/a-aaaaaa", Machine: "pc"}); got != "*pc/a-aaaaaa* · pc" {
		t.Fatalf("unnamed header = %q", got)
	}
}
