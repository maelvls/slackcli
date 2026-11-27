package later

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/slack/channels"
	"github.com/maelvls/slackcli/internal/slackapi"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func clock(t time.Time) func() time.Time { return func() time.Time { return t } }

func namedLater(limited bool) func(method string, params map[string]any) (map[string]any, error) {
	return func(method string, params map[string]any) (map[string]any, error) {
		switch method {
		case "saved.list":
			return savedList([4]float64{2, 0, 0, 2}, saved("C0GENERAL1", "1.1", "in_progress", 200), saved("C0RANDOM01", "2.2", "in_progress", 100)), nil
		case "conversations.info":
			if limited {
				return nil, &slackapi.RateLimitError{Method: method, RetryAfter: time.Minute}
			}
			return map[string]any{"channel": map[string]any{"name": "name-" + params["channel"].(string)}}, nil
		}
		return map[string]any{"messages": []any{map[string]any{"ts": params["latest"], "text": "m"}}}, nil
	}
}

func TestListCachesNames(t *testing.T) {
	dir := t.TempDir()
	var results []ListResult
	for run := range 2 {
		var infos atomic.Int32
		serve := namedLater(false)
		client := slackapi.NewFake(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
			if method == "conversations.info" {
				infos.Add(1)
			}
			return serve(method, params)
		})
		names := channels.NewNames(channels.NamesOptions{CacheDir: dir, Now: clock(t0.Add(time.Duration(run) * time.Hour))})
		res, err := List(context.Background(), client, ListOptions{Names: names})
		if err != nil {
			t.Fatal(err)
		}
		if want := []int32{2, 0}[run]; infos.Load() != want {
			t.Errorf("run %d: %d conversations.info calls, want %d", run, infos.Load(), want)
		}
		results = append(results, res)
	}
	if !reflect.DeepEqual(results[0], results[1]) || results[1].Items[0].ChannelName != "name-C0GENERAL1" {
		t.Errorf("looked up: %+v\nfrom the cache: %+v", results[0], results[1])
	}
}

// A rate-limited name lookup falls back on the stale cached name; only
// the channels left without any name are warned about.
func TestListRateLimitedNamesUseStaleEntries(t *testing.T) {
	dir := t.TempDir()
	old := channels.NewNames(channels.NamesOptions{CacheDir: dir, WorkspaceURL: "https://test.slack.com", Now: clock(t0.Add(-48 * time.Hour))})
	old.Remember(nil, "C0GENERAL1", "general")

	serve := namedLater(true)
	client := slackapi.NewFake(func(_ context.Context, method string, params map[string]any) (map[string]any, error) {
		return serve(method, params)
	})
	res, err := List(context.Background(), client, ListOptions{Names: channels.NewNames(channels.NamesOptions{CacheDir: dir, Now: clock(t0)})})
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].ChannelName != "general" || res.Items[1].ChannelName != "" {
		t.Errorf("items = %+v", res.Items)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "the names of 1 of 2 channels were not looked up") {
		t.Errorf("warnings = %q", res.Warnings)
	}
}
