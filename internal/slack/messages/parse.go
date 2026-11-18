// Package messages fetches Slack messages (one message, a channel's
// history, a thread) and turns them into the compact shape the CLI prints.
package messages

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slack/enrich"
	"github.com/maelvls/slackcli/internal/slack/slacktext"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// FileSummary describes a file attached to a message.
type FileSummary struct {
	ID                 string `json:"id"`
	Name               string `json:"name,omitempty"`
	Title              string `json:"title,omitempty"`
	Mimetype           string `json:"mimetype,omitempty"`
	Filetype           string `json:"filetype,omitempty"`
	Mode               string `json:"mode,omitempty"`
	Permalink          string `json:"permalink,omitempty"`
	URLPrivate         string `json:"url_private,omitempty"`
	URLPrivateDownload string `json:"url_private_download,omitempty"`
	Size               int64  `json:"size,omitempty"`
	// Snippet is the inline content of a snippet, when files.info has it.
	Snippet *FileSnippet `json:"snippet,omitempty"`
}

// FileSnippet is the text of a snippet file.
type FileSnippet struct {
	Content  string `json:"content,omitempty"`
	Language string `json:"language,omitempty"`
}

// Message is a fetched Slack message. Blocks, Attachments and Reactions
// are the raw Slack payloads.
type Message struct {
	ChannelID string `json:"channel_id"`
	TS        string `json:"ts"`
	// ThreadTS is the thread root ts, for thread roots and replies.
	ThreadTS   string `json:"thread_ts,omitempty"`
	ReplyCount int    `json:"reply_count,omitempty"`
	// Subtype is Slack's message subtype ("channel_join", "bot_message",
	// …); "" for an ordinary message. See IsSystem.
	Subtype string `json:"subtype,omitempty"`
	User    string `json:"user,omitempty"`
	BotID   string `json:"bot_id,omitempty"`
	// Text is the raw mrkdwn text; Markdown is its Markdown conversion.
	Text        string        `json:"text"`
	Markdown    string        `json:"markdown"`
	Blocks      []any         `json:"blocks,omitempty"`
	Attachments []any         `json:"attachments,omitempty"`
	Files       []FileSummary `json:"files,omitempty"`
	Reactions   []any         `json:"reactions,omitempty"`
}

// systemSubtypes are the subtypes of the messages Slack writes itself
// about a conversation: nobody said anything.
var systemSubtypes = []string{
	"channel_join", "channel_leave", "channel_topic", "channel_purpose", "channel_name", "channel_archive", "channel_unarchive",
	"group_join", "group_leave", "group_topic", "group_purpose", "group_name", "group_archive", "group_unarchive",
}

// IsSystem reports whether a message subtype is that of a system message
// (joins, leaves, topic, purpose and name changes, archiving). In a busy
// public channel these outnumber what people wrote, so the commands that
// read a conversation leave them out unless asked.
func IsSystem(subtype string) bool { return slices.Contains(systemSubtypes, subtype) }

// ParseMessage converts a raw Slack message object. Files are summarised
// as-is: no files.info lookup is made (the Fetch functions do that).
func ParseMessage(channelID string, raw map[string]any) Message {
	text := jsonx.Str(raw["text"])
	replyCount, _ := jsonx.Int(raw["reply_count"])
	msg := Message{
		ChannelID:   channelID,
		TS:          jsonx.Str(raw["ts"]),
		ThreadTS:    jsonx.Str(raw["thread_ts"]),
		ReplyCount:  replyCount,
		Subtype:     jsonx.Str(raw["subtype"]),
		User:        jsonx.Str(raw["user"]),
		BotID:       jsonx.Str(raw["bot_id"]),
		Text:        text,
		Markdown:    slacktext.MrkdwnToMarkdown(text),
		Blocks:      jsonx.Slice(raw["blocks"]),
		Attachments: jsonx.Slice(raw["attachments"]),
		Reactions:   jsonx.Slice(raw["reactions"]),
	}
	for _, f := range jsonx.Slice(raw["files"]) {
		if summary, ok := ParseFile(f); ok {
			msg.Files = append(msg.Files, summary)
		}
	}
	return msg
}

// ParseFile summarises a raw Slack file object. ok is false when v is not
// a file object with an ID.
func ParseFile(v any) (summary FileSummary, ok bool) {
	f := jsonx.Map(v)
	id := jsonx.Str(f["id"])
	if id == "" {
		return FileSummary{}, false
	}
	size, _ := jsonx.Int64(f["size"])
	return FileSummary{
		ID:                 id,
		Name:               jsonx.Str(f["name"]),
		Title:              jsonx.Str(f["title"]),
		Mimetype:           jsonx.Str(f["mimetype"]),
		Filetype:           jsonx.Str(f["filetype"]),
		Mode:               jsonx.Str(f["mode"]),
		Permalink:          jsonx.Str(f["permalink"]),
		URLPrivate:         jsonx.Str(f["url_private"]),
		URLPrivateDownload: jsonx.Str(f["url_private_download"]),
		Size:               size,
	}, true
}

// filesInfoConcurrency bounds the files.info lookups of enrichFiles.
const filesInfoConcurrency = 4

// needsInfo reports whether the file object embedded in a message is
// incomplete: snippets carry their content only in files.info, and files
// shared from elsewhere often come without a download URL.
func needsInfo(f FileSummary) bool { return f.Mode == "snippet" || f.URLPrivateDownload == "" }

// enrichFiles completes the files of msgs with files.info. Each file is
// looked up once however many messages share it; a failed lookup leaves
// the summary as it was.
//
// It is an enrichment (see package enrich): the error is only for a
// cancelled context or dead credentials. When files.info is rate limited
// no further file is asked for, the messages are returned with the file
// summaries they came with, and the collector of ctx is told.
func enrichFiles(ctx context.Context, client *slackapi.Client, msgs []Message) error {
	var ids []string
	infos := map[string]map[string]any{}
	for _, m := range msgs {
		for _, f := range m.Files {
			if _, seen := infos[f.ID]; needsInfo(f) && !seen {
				infos[f.ID] = nil
				ids = append(ids, f.ID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}

	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		fatal error
		sem   = make(chan struct{}, filesInfoConcurrency)
		// limited stops the lookups once Slack has refused one.
		limited enrich.Gate
	)
	for _, id := range ids {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			mu.Lock()
			abort := fatal != nil
			mu.Unlock()
			if abort || limited.Closed() {
				return
			}
			resp, err := client.API(ctx, "files.info", map[string]any{"file": id})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				infos[id] = jsonx.Map(resp["file"])
			case enrich.MustAbort(ctx, err):
				fatal = cmp.Or(fatal, err)
			default:
				limited.Failed(err)
			}
		})
	}
	wg.Wait()
	if fatal != nil {
		return fatal
	}
	if skipped, rateLimit := limited.Skipped(); skipped > 0 {
		enrich.Warnf(ctx, "the details of %d of %d attached files could not be fetched (%v): snippets are shown without their content and some files without a download URL; %s",
			skipped, len(ids), enrich.Cause(rateLimit), enrich.Hint())
	}

	for i := range msgs {
		for j, f := range msgs[i].Files {
			if info := infos[f.ID]; info != nil {
				msgs[i].Files[j] = mergeFileInfo(f, info)
			}
		}
	}
	return nil
}

func mergeFileInfo(f FileSummary, info map[string]any) FileSummary {
	fill := func(dst *string, key string) {
		if *dst == "" {
			*dst = jsonx.Str(info[key])
		}
	}
	fill(&f.Name, "name")
	fill(&f.Title, "title")
	fill(&f.Mimetype, "mimetype")
	fill(&f.Filetype, "filetype")
	fill(&f.Mode, "mode")
	fill(&f.Permalink, "permalink")
	fill(&f.URLPrivate, "url_private")
	fill(&f.URLPrivateDownload, "url_private_download")
	if content := jsonx.Str(info["content"]); strings.TrimSpace(content) != "" {
		f.Snippet = &FileSnippet{Content: content, Language: jsonx.Str(info["filetype"])}
	}
	return f
}
