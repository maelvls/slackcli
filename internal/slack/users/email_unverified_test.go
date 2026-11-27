package users

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Slack hides the email of many members from browser credentials, so that
// users.info cannot confirm what the people search found. "user get" (and
// it alone: GetUserOptions.AllowUnverifiedEmail) then takes the one member
// the search found, marked as unverified; the callers that act on a user
// keep refusing the match.
func TestUnverifiedEmailMatch(t *testing.T) {
	ctx := context.Background()
	alice := member("U00000001A", "alice", "profile", profile("email", "alice@example.com", "real_name", "Alice A"))
	alicia := member("U00000002A", "alicia", "profile", profile("email", "alicia@example.com"))
	hidden := member("U00000003A", "hidden", "tz", "Europe/Paris", "profile", profile("real_name", "Hidden H"))
	hidden2 := member("U00000004A", "hidden2", "profile", profile("real_name", "Hidden Too"))
	allow := GetUserOptions{AllowUnverifiedEmail: true}
	single := peopleWorkspace{items: []map[string]any{person("U00000003A", "hidden", "Hidden H", "")}, info: []map[string]any{hidden}}

	t.Run("one match with a hidden email: user get takes it, marked unverified", func(t *testing.T) {
		rec := single.recorder()
		got, err := GetUser(ctx, rec.client(), "hidden@example.com", allow)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != "U00000003A" || got.TZ != "Europe/Paris" || got.Email != "" || got.EmailVerified == nil || *got.EmailVerified {
			t.Errorf("GetUser = %+v, want U00000003A from users.info with EmailVerified false", got)
		}
		if want := []string{"search.modules.people", "users.info"}; !reflect.DeepEqual(rec.methods(), want) {
			t.Errorf("methods = %v, want %v", rec.methods(), want)
		}
	})

	// The same input, for every caller that does not ask for it.
	isUnverifiable := func(t *testing.T, err error) {
		t.Helper()
		var amb *AmbiguousError
		if !errors.As(err, &amb) || len(amb.Matches) != 1 || amb.Matches[0].ID != "U00000003A" || !strings.Contains(err.Error(), "cannot be checked") {
			t.Errorf("err = %v, want the AmbiguousError that names U00000003A", err)
		}
	}
	t.Run("user get without the option still refuses it", func(t *testing.T) {
		_, err := GetUser(ctx, single.recorder().client(), "hidden@example.com", GetUserOptions{})
		isUnverifiable(t, err)
	})
	t.Run("ResolveUserID (search --user, channel new --user) still refuses it", func(t *testing.T) {
		_, err := ResolveUserID(ctx, single.recorder().client(), "hidden@example.com")
		isUnverifiable(t, err)
	})
	t.Run("ResolveUserIDs (channel invite) still refuses it", func(t *testing.T) {
		got, err := ResolveUserIDs(ctx, single.recorder().client(), []string{"hidden@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].ID != "" {
			t.Errorf("ID = %q, want none", got[0].ID)
		}
		isUnverifiable(t, got[0].Err)
	})
	t.Run("GetDMChannelForUsers (user dm-open) still refuses it, before opening anything", func(t *testing.T) {
		rec := single.recorder()
		_, err := GetDMChannelForUsers(ctx, rec.client(), []string{"hidden@example.com"})
		isUnverifiable(t, err)
		if n := len(rec.only("conversations.open")); n != 0 {
			t.Errorf("conversations.open called %d times", n)
		}
	})

	t.Run("one match that users.info shows with another email is not the user, even for user get", func(t *testing.T) {
		rec := peopleWorkspace{
			items: []map[string]any{person("U00000002A", "alicia", "Alicia", "")},
			info:  []map[string]any{alicia},
			pages: [][]map[string]any{{alicia}},
		}.recorder()
		_, err := GetUser(ctx, rec.client(), "alice@example.com", allow)
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("err = %v, want a NotFoundError", err)
		}
	})

	t.Run("two matches with hidden emails: user get still refuses to pick", func(t *testing.T) {
		rec := peopleWorkspace{
			items: []map[string]any{person("U00000003A", "hidden", "Hidden H", ""), person("U00000004A", "hidden2", "Hidden Too", "")},
			info:  []map[string]any{hidden, hidden2},
		}.recorder()
		_, err := GetUser(ctx, rec.client(), "hidden@example.com", allow)
		var amb *AmbiguousError
		if !errors.As(err, &amb) || len(amb.Matches) != 2 {
			t.Errorf("err = %v, want an AmbiguousError with 2 matches", err)
		}
	})

	t.Run("a hidden email next to another search result: user get still refuses it", func(t *testing.T) {
		// The other member turns out to have another address, but the
		// search did not find one member only: the rule is one result
		// in all.
		rec := peopleWorkspace{
			items: []map[string]any{person("U00000003A", "hidden", "Hidden H", ""), person("U00000002A", "alicia", "Alicia", "")},
			info:  []map[string]any{hidden, alicia},
		}.recorder()
		_, err := GetUser(ctx, rec.client(), "hidden@example.com", allow)
		isUnverifiable(t, err)
	})

	t.Run("a single match users.info refuses: user get still fails", func(t *testing.T) {
		rec := peopleWorkspace{items: []map[string]any{person("U00000003A", "hidden", "", "")}}.recorder()
		got, err := GetUser(ctx, rec.client(), "hidden@example.com", allow)
		if err == nil || got.ID != "" || !strings.Contains(err.Error(), "users.info cannot show it") {
			t.Errorf("got %+v, err = %v", got, err)
		}
	})

	t.Run("a single bot without email: user get still refuses it", func(t *testing.T) {
		bot := member("U00000005A", "deploy", "is_bot", true, "profile", profile("real_name", "Deploy"))
		rec := peopleWorkspace{items: []map[string]any{person("U00000005A", "deploy", "Deploy", "")}, info: []map[string]any{bot}}.recorder()
		got, err := GetUser(ctx, rec.client(), "deploy@example.com", allow)
		var amb *AmbiguousError
		if !errors.As(err, &amb) || got.ID != "" {
			t.Errorf("got %+v, err = %v, want an AmbiguousError", got, err)
		}
	})

	t.Run("a confirmed match carries no marker", func(t *testing.T) {
		rec := peopleWorkspace{items: []map[string]any{person("U00000001A", "alice", "Alice A", "")}, info: []map[string]any{alice}}.recorder()
		got, err := GetUser(ctx, rec.client(), "alice@example.com", allow)
		if err != nil || got.ID != "U00000001A" || got.EmailVerified != nil {
			t.Errorf("got %+v, %v, want U00000001A without EmailVerified", got, err)
		}
	})
}
