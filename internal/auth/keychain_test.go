package auth

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
)

func TestSecurityQuote(t *testing.T) {
	for in, want := range map[string]string{
		``:                 `""`,
		`plain`:            `"plain"`,
		`two words`:        `"two words"`,
		"tab\there":        "\"tab\there\"",
		`say "hi"`:         `"say \"hi\""`,
		`it's`:             `"it's"`,
		`back\slash`:       `"back\\slash"`,
		`trailing\`:        `"trailing\\"`,
		`\"`:               `"\\\""`,
		`-w`:               `"-w"`,
		`--`:               `"--"`,
		`é ü 日本`:           `"é ü 日本"`,
		`$HOME; x | y #z`:  `"$HOME; x | y #z"`,
		"line\nbreak":      "\"line\nbreak\"", // not escaped: addPasswordCommand refuses it
		`a" -A -s "other`:  `"a\" -A -s \"other"`,
		`token:https://a/`: `"token:https://a/"`,
	} {
		if got := securityQuote(in); got != want {
			t.Errorf("securityQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// securityBuffer is the size of the buffer in which `security -i` reads a
// line: a longer line is cut there, and the rest is the next command.
const securityBuffer = 4096

// securityCommands is a model of how `security -i` reads its input, as
// observed with its harmless "help" command on macOS 15 ("help \"a\\\"b c\""
// answers `no such command: a"b c`): one command per line, a line cut every
// securityBuffer-1 bytes, words separated by blanks, a word in single or
// double quotes ending at the same quote, and a backslash making the next
// character literal everywhere.
func securityCommands(stdin string) [][]string {
	var commands [][]string
	for _, line := range strings.Split(strings.TrimSuffix(stdin, "\n"), "\n") {
		for len(line) > securityBuffer-1 {
			commands = append(commands, securityWords(line[:securityBuffer-1]))
			line = line[securityBuffer-1:]
		}
		commands = append(commands, securityWords(line))
	}
	return commands
}

func securityWords(line string) []string {
	var words []string
	var word []byte
	inWord, quote := false, byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && i+1 < len(line):
			i++
			word, inWord = append(word, line[i]), true
		case quote != 0 && c == quote:
			words, word, inWord, quote = append(words, string(word)), nil, false, 0
		case quote != 0:
			word = append(word, c)
		case !inWord && (c == '"' || c == '\''):
			inWord, quote = true, c
		case c == ' ' || c == '\t' || c == '\r':
			if inWord {
				words, word, inWord = append(words, string(word)), nil, false
			}
		default:
			word, inWord = append(word, c), true
		}
	}
	if inWord {
		words = append(words, string(word))
	}
	return words
}

// Whatever the service, the account and the secret hold, `security -i` must
// see exactly one command, with each of them as one argument: none of them
// may end the command, add an option to it or start another command.
func TestAddPasswordCommandCannotBeInjected(t *testing.T) {
	nasty := []string{
		"xoxc-123-abc",
		"two words",
		"tab\there",
		` leading and trailing `,
		`say "hi"`,
		`it's`,
		`'single'`,
		`back\slash`,
		`trailing\`,
		`\`,
		`\\`,
		`\"`,
		`"`,
		`" -A -T "" -s "other`,
		`x" -w "other`,
		`-w`,
		`-U`,
		`--`,
		`é ü 日本 🙂`,
		`$HOME $(id) ` + "`id`" + ` ; delete-keychain | #`,
		strings.Repeat("a", 3000),
	}
	for _, value := range nasty {
		for _, fields := range [][3]string{
			{"svc", "token:https://acme.slack.com", value},
			{"svc", value, "secret"},
			{value, "account", "secret"},
		} {
			service, account, secret := fields[0], fields[1], fields[2]
			command, err := addPasswordCommand(service, account, secret)
			if err != nil {
				t.Errorf("addPasswordCommand(%q, %q, %q) refused: %v", service, account, secret, err)
				continue
			}
			want := [][]string{{"add-generic-password", "-U", "-s", service, "-a", account, "-w", secret}}
			if got := securityCommands(command); !reflect.DeepEqual(got, want) {
				t.Errorf("addPasswordCommand(%q, %q, %q) is read by security as\n%q\nwant\n%q", service, account, secret, got, want)
			}
		}
	}
}

// What cannot stay on one line that fits the buffer of `security -i` would
// be run as a second command: it is refused, without quoting the value.
func TestAddPasswordCommandRefusesWhatCannotBeQuoted(t *testing.T) {
	const marker = "s3cr3t-marker"
	for name, value := range map[string]string{
		"newline":              marker + "\ndelete-keychain",
		"carriage return":      marker + "\rdelete-keychain",
		"NUL":                  marker + "\x00",
		"too long":             marker + strings.Repeat("a", securityBuffer) + " delete-keychain",
		"too long once quoted": marker + strings.Repeat(`"`, securityBuffer/2),
	} {
		for _, fields := range [][3]string{{"svc", "account", value}, {"svc", value, "secret"}, {value, "account", "secret"}} {
			command, err := addPasswordCommand(fields[0], fields[1], fields[2])
			if err == nil {
				t.Errorf("%s: accepted, and read by security as %d commands", name, len(securityCommands(command)))
				continue
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("%s: the error quotes the value: %v", name, err)
			}
		}
	}

	// The limit leaves room: the longest command accepted is one line for
	// security.
	longest := strings.Repeat("a", maxSecurityCommand-len(`add-generic-password -U -s "s" -a "a" -w ""`+"\n"))
	command, err := addPasswordCommand("s", "a", longest)
	if err != nil || len(command) != maxSecurityCommand || len(securityCommands(command)) != 1 {
		t.Errorf("the longest secret: command of %d bytes, %v", len(command), err)
	}
	if _, err := addPasswordCommand("s", "a", longest+"a"); err == nil {
		t.Error("a command longer than the limit was accepted")
	}
	if maxSecurityCommand >= securityBuffer {
		t.Errorf("the limit (%d) does not fit the buffer of security (%d)", maxSecurityCommand, securityBuffer)
	}
}

// securityCall is one run of security(1) as a fake runner saw it.
type securityCall struct {
	stdin       string
	args        []string
	hadDeadline bool
}

// fakeSecurity returns a keychain whose security(1) answers with result,
// and the calls it received.
func fakeSecurity(result securityResult) (macKeychain, *[]securityCall) {
	calls := new([]securityCall)
	return macKeychain{run: func(ctx context.Context, stdin string, args ...string) securityResult {
		_, hadDeadline := ctx.Deadline()
		*calls = append(*calls, securityCall{stdin: stdin, args: args, hadDeadline: hadDeadline})
		return result
	}}, calls
}

// hangingSecurity is a security(1) that waits on a prompt until it is
// killed.
func hangingSecurity(ctx context.Context, _ string, _ ...string) securityResult {
	<-ctx.Done()
	return securityResult{err: ctx.Err()}
}

func TestMacKeychainGet(t *testing.T) {
	cases := []struct {
		name    string
		result  securityResult
		want    string
		wantErr string
	}{
		{name: "found", result: securityResult{stdout: []byte(" xoxc-1\n")}, want: "xoxc-1"},
		{name: "not found", result: securityResult{exit: securityItemNotFound, stderr: []byte("security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.\n")}},
		{name: "locked", result: securityResult{exit: 36, stderr: []byte("security: User interaction is not allowed.\n")}, wantErr: "security exited with status 36"},
		{name: "not started", result: securityResult{err: errors.New("exec: no security")}, wantErr: "could not be read: exec: no security"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kc, calls := fakeSecurity(c.result)
			got, err := kc.Get("svc", "xoxc:https://acme.slack.com")
			if got != c.want {
				t.Errorf("Get = %q, want %q", got, c.want)
			}
			if (c.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.wantErr)) {
				t.Errorf("err = %v, want %q", err, c.wantErr)
			}
			want := []securityCall{{args: []string{"find-generic-password", "-s", "svc", "-a", "xoxc:https://acme.slack.com", "-w"}, hadDeadline: true}}
			if !reflect.DeepEqual(*calls, want) {
				t.Errorf("calls = %+v, want %+v", *calls, want)
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		t.Setenv(appinfo.EnvName("KEYCHAIN_TIMEOUT_MS"), "20")
		start := time.Now()
		_, err := macKeychain{run: hangingSecurity}.Get("svc", "account")
		if err == nil || !strings.Contains(err.Error(), "did not answer within 20ms") {
			t.Errorf("err = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("Get waited %s", elapsed)
		}
	})
}

func TestMacKeychainSet(t *testing.T) {
	const secret = `xoxc-s3cr3t "quoted" \ end`
	const account = "xoxc:https://acme.slack.com"

	t.Run("the secret goes to stdin, never to argv", func(t *testing.T) {
		kc, calls := fakeSecurity(securityResult{})
		if err := kc.Set("svc", account, secret); err != nil {
			t.Fatal(err)
		}
		want := []securityCall{{
			stdin:       `add-generic-password -U -s "svc" -a "xoxc:https://acme.slack.com" -w "xoxc-s3cr3t \"quoted\" \\ end"` + "\n",
			args:        []string{"-i"},
			hadDeadline: true,
		}}
		if !reflect.DeepEqual(*calls, want) {
			t.Errorf("calls = %+v, want %+v", *calls, want)
		}
	})

	failures := []struct {
		name    string
		result  securityResult
		wantErr string
	}{
		{
			name: "the sub-command fails although security exits 0",
			// security can quote its input in what it writes.
			result:  securityResult{stderr: []byte("security: SecKeychainItemCreateFromContent (<default>): User interaction is not allowed. " + secret + "\nadd-generic-password: returned 36\n")},
			wantErr: "security add-generic-password returned status 36",
		},
		{name: "exit status alone", result: securityResult{exit: 1}, wantErr: "security exited with status 1"},
		{name: "stderr without a status", result: securityResult{stderr: []byte("security: unknown command \"" + secret + "\"\n")}, wantErr: "reported an error"},
		{name: "not started", result: securityResult{err: errors.New("exec: no security")}, wantErr: "could not be written: exec: no security"},
	}
	for _, c := range failures {
		t.Run(c.name, func(t *testing.T) {
			kc, _ := fakeSecurity(c.result)
			err := kc.Set("svc", account, secret)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("the error holds the secret: %v", err)
			}
		})
	}

	t.Run("a value that could start another command never reaches security", func(t *testing.T) {
		kc, calls := fakeSecurity(securityResult{})
		for _, value := range []string{"s3cr3t\ndelete-keychain", "s3cr3t" + strings.Repeat("a", securityBuffer)} {
			err := kc.Set("svc", account, value)
			if err == nil || strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("err = %v", err)
			}
		}
		if len(*calls) != 0 {
			t.Errorf("security was run: %+v", *calls)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Setenv(appinfo.EnvName("KEYCHAIN_TIMEOUT_MS"), "20")
		err := macKeychain{run: hangingSecurity}.Set("svc", account, secret)
		if err == nil || !strings.Contains(err.Error(), "did not answer within 20ms") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestMacKeychainDelete(t *testing.T) {
	// Nothing to delete is not an error: Delete is best effort.
	kc, calls := fakeSecurity(securityResult{exit: securityItemNotFound})
	kc.Delete("svc", "token:https://acme.slack.com")
	want := []securityCall{{args: []string{"delete-generic-password", "-s", "svc", "-a", "token:https://acme.slack.com"}, hadDeadline: true}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %+v, want %+v", *calls, want)
	}
}
