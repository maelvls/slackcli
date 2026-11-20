package messagecmd

import "testing"

// --text is the agent-slack spelling of [text]: it sends the same message
// as the positional argument, and is left out of the help.
func TestSendTextFlag(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"send", testChannel, "--text", "hello"},
		{"send", testChannel, "--text=hello"},
		{"send", "--text", "hello", testChannel},
	} {
		h.mustRun(args...)
		h.wantCalls(call{"chat.postMessage", map[string]any{"channel": testChannel, "text": "hello"}})
	}

	// Like [text], it may start with a dash and is converted to a list.
	h.mustRun("send", testChannel, "--text", "- one\n- two")
	calls := h.callsTo("chat.postMessage")
	if len(calls) != 1 || calls[0].params["text"] != "- one\n- two" || calls[0].params["blocks"] == nil {
		t.Errorf("chat.postMessage calls = %+v", calls)
	}

	if f := findCommand(t, New(h.rt), "send").Flags().Lookup("text"); f == nil || !f.Hidden {
		t.Errorf("--text = %+v, want a hidden flag", f)
	}
}

func TestSendTextFlagErrors(t *testing.T) {
	h := newHarness(t)
	h.mustFail("the text is given twice, as [text] and with --text", "send", testChannel, "hello", "--text", "world")
	h.mustFail("[text] is required unless --attach or --blocks is given", "send", testChannel, "--text", "")
	h.mustFail("[text] is required unless --attach or --blocks is given", "send", testChannel, "--text", "  ")
	h.wantCalls()
}

// In safe mode the text of --text goes to the compose editor, like [text].
func TestSendTextFlagInSafeMode(t *testing.T) {
	h := newHarness(t)
	h.rt.SafeModeFlag = true
	h.mustRun("send", testChannel, "--text", "hello")
	if len(h.composed) != 1 || h.composed[0].text != "hello" {
		t.Errorf("composed = %+v", h.composed)
	}
	h.wantCalls()
}
