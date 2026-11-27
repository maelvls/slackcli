// Package upload sends local files to Slack with the two-step external
// upload flow: files.getUploadURLExternal reserves a file ID and a
// pre-signed URL, the bytes are POSTed to that URL, and
// files.completeUploadExternal finalises the files (optionally sharing
// them to a conversation).
//
// Every function validates all paths and stages all files before
// completing anything: Slack discards staged-but-uncompleted uploads, so a
// failure half-way through leaves nothing behind.
package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/audit"
	"github.com/maelvls/slackcli/internal/jsonx"
	"github.com/maelvls/slackcli/internal/slackapi"
)

// MaxFileSize is Slack's documented per-file upload limit (1 GB).
const MaxFileSize int64 = 1 << 30

// cleanupTimeout bounds DeleteFiles, which deliberately outlives the
// caller's (possibly already cancelled) context.
const cleanupTimeout = 15 * time.Second

// File is a file uploaded to Slack.
type File struct {
	// ID is Slack's file ID (F…).
	ID string `json:"id"`
	// Name is the file name Slack was given (base name of the local path,
	// symlinks resolved).
	Name string `json:"name"`
	// Size is the number of bytes uploaded.
	Size int64 `json:"size"`
}

// IDs returns the Slack file IDs of files, in order.
func IDs(files []File) []string {
	ids := make([]string, len(files))
	for i, f := range files {
		ids[i] = f.ID
	}
	return ids
}

// Destination is where ToChannel shares the uploaded files.
type Destination struct {
	// ChannelID is the conversation ID (C…, D…, G…). Required.
	ChannelID string
	// ThreadTS, when set, posts the files as a reply in that thread.
	ThreadTS string
	// InitialComment is the message text posted with the files (plain
	// text/mrkdwn; Slack does not accept blocks here). Optional.
	InitialComment string
}

// Check validates paths without talking to Slack: each must exist, be a
// regular non-empty file and be at most MaxFileSize. Commands call it
// before doing any other work (resolving channels, opening DMs …) so that a
// bad --attach fails fast. ToChannel and ForDraft run the same checks
// again.
func Check(paths []string) error {
	for _, p := range paths {
		if _, err := inspect(p); err != nil {
			return err
		}
	}
	return nil
}

// ToChannel uploads the files at paths and shares them to dest as ONE
// message carrying dest.InitialComment. Nothing is posted unless every
// file was validated and staged successfully.
func ToChannel(ctx context.Context, client *slackapi.Client, dest Destination, paths []string) ([]File, error) {
	if dest.ChannelID == "" {
		return nil, errors.New("uploading files: no destination channel ID")
	}
	params := map[string]any{"channel_id": dest.ChannelID}
	if dest.ThreadTS != "" {
		params["thread_ts"] = dest.ThreadTS
	}
	if c := strings.TrimSpace(dest.InitialComment); c != "" {
		params["initial_comment"] = c
	}
	return uploadAll(ctx, client, paths, params)
}

// ForDraft uploads the files at paths without sharing them anywhere: they
// stay private to the user until a draft referencing their IDs is sent.
// If the draft call that was meant to reference them then fails, pass
// IDs(files) to DeleteFiles so they do not linger as orphans.
func ForDraft(ctx context.Context, client *slackapi.Client, paths []string) ([]File, error) {
	return uploadAll(ctx, client, paths, map[string]any{})
}

// DeleteFiles deletes uploaded files that ended up unused (see ForDraft).
// Because it typically runs after another call failed — possibly because
// ctx was cancelled or timed out — it detaches from ctx's cancellation and
// applies its own short timeout. Files that are already gone are not an
// error. The returned error joins the individual failures; callers for
// whom this is best-effort cleanup should report it as a hint rather than
// let it mask the original error.
func DeleteFiles(ctx context.Context, client *slackapi.Client, fileIDs []string) error {
	if len(fileIDs) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	errs := make([]error, len(fileIDs))
	var wg sync.WaitGroup
	for i, id := range fileIDs {
		wg.Go(func() {
			_, err := client.API(ctx, "files.delete", map[string]any{"file": id})
			if err != nil && !slackapi.IsErrorCode(err, "file_not_found", "file_deleted") {
				errs[i] = fmt.Errorf("deleting uploaded file %s: %w", id, err)
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// uploadAll validates and stages every path, then finalises them with a
// single files.completeUploadExternal call carrying completeParams.
func uploadAll(ctx context.Context, client *slackapi.Client, paths []string, completeParams map[string]any) ([]File, error) {
	if len(paths) == 0 {
		return nil, errors.New("uploading files: no file paths given")
	}
	// Validate everything first: a typo in the last path must not cost the
	// upload of all the files before it.
	locals := make([]localFile, len(paths))
	for i, p := range paths {
		l, err := inspect(p)
		if err != nil {
			return nil, err
		}
		locals[i] = l
	}

	files := make([]File, len(locals))
	for i, l := range locals {
		f, err := stage(ctx, client, l)
		if err != nil {
			return nil, err
		}
		files[i] = f
	}

	refs := make([]map[string]any, len(files))
	for i, f := range files {
		refs[i] = map[string]any{"id": f.ID, "title": f.Name}
	}
	completeParams["files"] = refs
	resp, err := client.API(ctx, "files.completeUploadExternal", completeParams)
	if err != nil {
		return nil, fmt.Errorf("finalising upload of %s: %s%w", describe(files), unconfirmed(err, completeParams), err)
	}

	// The completion response is authoritative for the final IDs, but only
	// positionally, and only when it lines up 1:1 with what was sent:
	// guessing otherwise could attach the wrong file to a draft.
	if completed := jsonx.Slice(resp["files"]); len(completed) == len(files) {
		for i, c := range completed {
			if id := jsonx.GetStr(c, "id"); id != "" {
				files[i].ID = id
			}
		}
	}
	return files, nil
}

// unconfirmed is what to say before the error of a
// files.completeUploadExternal call whose outcome is unknown ("" when it
// is known). When Slack refuses the call (an API error, a rate limit)
// nothing was done. When no answer came back (a timeout, a dropped
// connection, a cancelled command), Slack may have processed the request
// all the same: the files are then shared, and a retry shares them again.
func unconfirmed(err error, completeParams map[string]any) string {
	var rl *slackapi.RateLimitError
	if slackapi.ErrorCode(err) != "" || errors.As(err, &rl) {
		return ""
	}
	if channel, _ := completeParams["channel_id"].(string); channel != "" {
		return fmt.Sprintf("Slack did not confirm it, so the upload may have gone through and the message may already be in %[1]s: "+
			"check with `%[2]s message list %[1]s --limit 5` before retrying, since a retry would post the files a second time: ", channel, appinfo.Name)
	}
	return "Slack did not confirm it, so the upload may have gone through and the files may exist in Slack, attached to nothing: "
}

type localFile struct {
	// given is the path as the user wrote it (for error messages);
	// resolved has symlinks followed.
	given, resolved string
}

func inspect(path string) (localFile, error) {
	if strings.TrimSpace(path) == "" {
		return localFile{}, errors.New("attachment path is empty")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return localFile{}, pathError(path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return localFile{}, pathError(path, err)
	}
	if err := checkInfo(path, info); err != nil {
		return localFile{}, err
	}
	return localFile{given: path, resolved: resolved}, nil
}

func checkInfo(path string, info fs.FileInfo) error {
	switch {
	case info.IsDir():
		return fmt.Errorf("attachment path is a directory, not a file: %s (attach files one by one, or archive the directory first)", path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("attachment path is not a regular file: %s", path)
	case info.Size() == 0:
		return fmt.Errorf("attachment is empty (0 bytes): %s (Slack rejects empty files)", path)
	case info.Size() > MaxFileSize:
		return fmt.Errorf("attachment is too large: %s is %s, Slack allows up to %s per file", path, humanSize(info.Size()), humanSize(MaxFileSize))
	}
	return nil
}

func pathError(path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("attachment not found: %s (relative paths resolve against the current directory)", path)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("attachment is not readable (permission denied): %s", path)
	}
	return fmt.Errorf("reading attachment %s: %w", path, err)
}

// stage reserves an upload URL for l and streams its bytes there.
func stage(ctx context.Context, client *slackapi.Client, l localFile) (File, error) {
	f, err := os.Open(l.resolved)
	if err != nil {
		return File{}, pathError(l.given, err)
	}
	defer f.Close()
	// Re-check on the open handle: the size announced to Slack must be the
	// size of the file actually streamed.
	info, err := f.Stat()
	if err != nil {
		return File{}, pathError(l.given, err)
	}
	if err := checkInfo(l.given, info); err != nil {
		return File{}, err
	}
	name, size := filepath.Base(l.resolved), info.Size()

	resp, err := client.API(ctx, "files.getUploadURLExternal", map[string]any{
		"filename": name,
		"length":   size,
	})
	if err != nil {
		return File{}, fmt.Errorf("reserving an upload for %s: %w", l.given, err)
	}
	uploadURL, fileID := jsonx.Str(resp["upload_url"]), jsonx.Str(resp["file_id"])
	if uploadURL == "" || fileID == "" {
		return File{}, fmt.Errorf("reserving an upload for %s: Slack returned no upload URL or file ID", l.given)
	}
	if err := postBytes(ctx, client, uploadURL, f, size); err != nil {
		return File{}, fmt.Errorf("uploading %s: %w", l.given, err)
	}
	return File{ID: fileID, Name: name, Size: size}, nil
}

// postBytes streams size bytes of body to the pre-signed uploadURL. The URL
// itself authorises the upload, so no token or cookie is attached: they
// must never leave Slack's API hosts, whatever host the response named.
func postBytes(ctx context.Context, client *slackapi.Client, uploadURL string, body io.Reader, size int64) error {
	u, err := url.Parse(uploadURL)
	if err != nil || u.Host == "" {
		return errors.New("Slack returned an invalid upload URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing to upload over %q: Slack returned a non-HTTPS upload URL", u.Scheme)
	}
	// The upload has no overall time limit (a large file on a slow link
	// takes as long as it takes), but it must not hang forever on a
	// connection that went dead: a half-open TCP connection, a captive
	// portal, a laptop put to sleep. The watchdog cancels the request when
	// no byte of the file was taken for idle, and, once all of it was
	// taken, when no answer came within tail after the last one. The tail
	// is longer: a byte taken is not a byte sent, and the last megabytes
	// may still sit in the socket buffers of a slow link, which drain with
	// nothing more to read, before Slack even starts to answer.
	idle := stallTimeout()
	tail := max(idle, stallTailMin)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	w := &stallWatch{r: body, idle: idle, tail: tail, size: size}
	w.timer = time.AfterFunc(idle, func() { cancel(errStalled) })
	defer w.timer.Stop()

	// NopCloser: the transport closes request bodies, and the caller owns
	// the file.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, io.NopCloser(w))
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", appinfo.UserAgent())

	// A copy of the client that follows no redirect. The pre-signed URL is
	// the only place the bytes are meant for, and a redirect could name
	// any host. Nor could following one upload anything: net/http turns a
	// redirected POST into a GET without a body (301, 302, 303), whose 200
	// would pass for a successful upload, and cannot replay a streamed
	// body (307, 308).
	hc := *client.HTTPClient()
	hc.Transport = audit.Transport(hc.Transport, "files.upload", false)
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil && errors.Is(context.Cause(ctx), errStalled) {
		return w.stalledError(u.Host)
	}
	if err != nil {
		// *url.Error embeds the full pre-signed URL; keep it out of
		// messages that end up in agent transcripts.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("sending file bytes to %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		// Only the host of the target: its path and query may be signed.
		target := "another location"
		if loc, err := url.Parse(resp.Header.Get("Location")); err == nil && loc.Host != "" {
			target = loc.Host
		}
		return fmt.Errorf("sending file bytes to %s: it answered with a redirect (HTTP %d) to %s, which was not followed: the contents of a file are only sent to the upload URL Slack reserved. Nothing was uploaded: run the command again", u.Host, resp.StatusCode, target)
	}
	// The answer came: the rest (a short body) gets one more idle period.
	w.timer.Reset(idle)
	snippet, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := fmt.Sprintf("sending file bytes to %s failed with HTTP %d", u.Host, resp.StatusCode)
		if s := strings.TrimSpace(string(snippet)); s != "" {
			msg += ": " + s
		}
		if readErr != nil && errors.Is(context.Cause(ctx), errStalled) {
			msg += fmt.Sprintf(" (the rest of its answer stalled for %s)", idle)
		}
		return errors.New(msg)
	}
	return nil
}

// errStalled is the cause of the cancellation of an upload whose
// connection made no progress for the stall timeout. It is a timeout
// (audit.ErrorClass logs the request as one, not as cancelled).
var errStalled error = stallCause{}

type stallCause struct{}

func (stallCause) Error() string { return "upload stalled" }
func (stallCause) Timeout() bool { return true }

// stallTimeout is how long an upload may go without progress: the time
// limit of one Slack request (slackapi.APITimeout). A variable for tests.
var stallTimeout = slackapi.APITimeout

// stallTailMin is the least time to wait for an answer after the last
// byte of the file was taken (the stall timeout when longer). A variable
// for tests.
var stallTailMin = 2 * time.Minute

// stallWatch passes the bytes of a file on to the transport, and pushes
// back the stall watchdog (timer) each time some are taken.
type stallWatch struct {
	r     io.Reader
	idle  time.Duration
	tail  time.Duration
	size  int64
	timer *time.Timer
	// sent is the number of bytes handed to the transport. The transport
	// may read the body from a goroutine of its own.
	sent atomic.Int64
}

func (w *stallWatch) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 || err == io.EOF {
		d := w.idle
		if w.sent.Add(int64(n)) >= w.size || err == io.EOF {
			d = w.tail
		}
		w.timer.Reset(d)
	}
	return n, err
}

// stalledError says that the upload to host stalled, where, and what to
// do about it.
func (w *stallWatch) stalledError(host string) error {
	sent := w.sent.Load()
	waited := w.idle
	where := fmt.Sprintf("after %s of %s were sent", humanSize(sent), humanSize(w.size))
	if sent >= w.size {
		waited = w.tail
		where = fmt.Sprintf("all %s were handed to the network but %s never answered", humanSize(w.size), host)
	}
	return fmt.Errorf("sending file bytes to %s: the upload stalled: no progress for %s (%s). "+
		"The connection is probably dead (network change, sleep, captive portal, VPN): "+
		"check the network and run the command again; nothing was posted. "+
		"%s sets how long to wait for progress (in milliseconds; the wait for an answer is at least %s)",
		host, waited, where, appinfo.EnvName("API_TIMEOUT_MS"), stallTailMin)
}

func describe(files []File) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Name
	}
	return strings.Join(names, ", ")
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}
