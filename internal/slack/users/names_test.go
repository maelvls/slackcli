package users

import (
	"testing"

	"github.com/maelvls/slackcli/internal/slack/messages"
)

func TestLabelPrefersTheFullName(t *testing.T) {
	for _, tc := range []struct {
		u    CompactUser
		want string
	}{
		{CompactUser{Name: "kraghani", DisplayName: "Karan", RealName: "Karan Raghani"}, "Karan Raghani"},
		{CompactUser{Name: "kraghani", DisplayName: "Karan"}, "Karan"},
		{CompactUser{Name: "kraghani"}, "kraghani"},
		{CompactUser{}, ""},
	} {
		if got := Label(tc.u); got != tc.want {
			t.Errorf("Label(%+v) = %q, want %q", tc.u, got, tc.want)
		}
	}
}

func TestNameMessages(t *testing.T) {
	byID := map[string]CompactUser{
		"U0AAAAAAAAA": {ID: "U0AAAAAAAAA", RealName: "Karan Raghani"},
		"U0BBBBBBBBB": {ID: "U0BBBBBBBBB", DisplayName: "Amrut"},
	}
	msgs := []messages.CompactMessage{
		{Author: &messages.Author{UserID: "U0AAAAAAAAA"}, Content: "@U0BBBBBBBBB and @U0CCCCCCCCC, see @U0AAAAAAAAA."},
		{Author: &messages.Author{BotID: "B0AAAAAAAAA", Name: "deploybot"}, Content: "no mention"},
	}
	NameMessages(msgs, byID)
	if got := msgs[0].Author.Name; got != "Karan Raghani" {
		t.Errorf("author name = %q", got)
	}
	if got, want := msgs[0].Content, "@Amrut and @U0CCCCCCCCC, see @Karan Raghani."; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if got := msgs[1].Author.Name; got != "deploybot" {
		t.Errorf("bot name = %q, want it kept", got)
	}
	if got := MentionedInContent("hi @U0AAAAAAAAA and @here"); len(got) != 1 || got[0] != "U0AAAAAAAAA" {
		t.Errorf("MentionedInContent = %v", got)
	}
}
