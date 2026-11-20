package messagecmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/maelvls/slackcli/internal/clictx"
	"github.com/maelvls/slackcli/internal/cmd/composecmd"
)

// Safe mode keeps a human in the loop for what an agent writes to Slack:
// "message send" is redirected to the compose editor; "message edit",
// "message delete" and "message scheduled cancel" are refused
// (clictx.Context.CheckSafeMode).

// redirectedResult is the output of a "send" that safe mode handed to the
// compose editor: what "compose" prints, after two fields that say so.
type redirectedResult struct {
	SafeMode       bool   `json:"safe_mode"`
	RedirectedFrom string `json:"redirected_from"`
	composecmd.Result
}

// redirectFlags says which of the send flags that carry a value were
// given (an explicitly empty value counts).
type redirectFlags struct {
	attach, blocks, schedule, scheduleIn bool
}

// redirectSendToCompose hands a "message send" to the compose editor, so
// that a human reviews and sends it. Flags the editor cannot represent are
// refused rather than silently dropped; so is CI, where the editor cannot
// open and compose would otherwise send straight away.
func (c *command) redirectSendToCompose(ctx context.Context, targetInput, text string, opts sendOptions, set redirectFlags) (redirectedResult, error) {
	var unsupported []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"--attach", set.attach},
		{"--blocks", set.blocks},
		{"--schedule", set.schedule},
		{"--schedule-in", set.scheduleIn},
		{"--reply-broadcast", opts.replyBroadcast},
	} {
		if f.set {
			unsupported = append(unsupported, f.name)
		}
	}
	if len(unsupported) > 0 {
		return redirectedResult{}, fmt.Errorf(`safe mode is active (turned on by %s): "message send" is redirected to the compose editor, which does not support %s. Drop those flags or disable safe mode`, c.rt.SafeModeSource(), strings.Join(unsupported, ", "))
	}
	if clictx.IsCI() {
		return redirectedResult{}, fmt.Errorf("safe mode is active (turned on by %s) but the interactive compose editor is unavailable in CI (the CI environment variable is set). Disable safe mode, or unset CI, to send messages", c.rt.SafeModeSource())
	}

	// In the result ("hints"), not on stderr: "message send … 2>&1 | jq"
	// must still parse once the human has sent the message.
	c.rt.ResultHintf(`Safe mode active (turned on by %s): "message send" was redirected to the compose editor. Nothing is posted unless a human sends it from the editor.`, c.rt.SafeModeSource())
	// The editor waits on a human for as long as it takes. It checks
	// --thread-ts against a message URL target as "send" does.
	c.rt.DisableTimeout()
	payload, err := c.compose(ctx, c.rt, targetInput, text, composecmd.Options{
		Workspace: opts.workspace,
		ThreadTS:  strings.TrimSpace(opts.threadTS),
		NoUnfurl:  opts.noUnfurl,
	})
	if err != nil {
		return redirectedResult{}, err
	}
	return redirectedResult{SafeMode: true, RedirectedFrom: "send", Result: payload}, nil
}
