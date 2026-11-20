package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/maelvls/slackcli/internal/appinfo"
	"github.com/maelvls/slackcli/internal/trace"
)

// Keychain stores secrets outside the credentials file. The only real
// implementation is the macOS login keychain; elsewhere secrets stay in the
// 0600 credentials file.
type Keychain interface {
	// Get returns the secret. An item that does not exist is ("", nil);
	// an error means that the keychain could not be read (locked, timed
	// out waiting on a prompt…), which says nothing about the item.
	Get(service, account string) (string, error)
	// Set stores the secret. The error says why the keychain did not take
	// it, and never contains the secret.
	Set(service, account, value string) error
	// Delete removes the secret (best effort).
	Delete(service, account string)
}

// SystemKeychain returns the platform keychain, or nil when there is none.
func SystemKeychain() Keychain {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if _, err := exec.LookPath("security"); err != nil {
		return nil
	}
	return macKeychain{run: runSecurity}
}

// macKeychain is the login keychain, driven through security(1).
type macKeychain struct {
	// run runs security(1); tests replace it.
	run securityRunner
}

// securityResult is the outcome of one run of security(1).
type securityResult struct {
	stdout, stderr []byte
	// exit is the exit status of a run that ended on its own.
	exit int
	// err says why there is no exit status: security(1) could not be
	// started, or was killed (by the timeout).
	err error
}

// securityRunner runs security(1) with args, feeding it stdin. args are
// visible in the process list: a secret only ever goes in stdin.
type securityRunner func(ctx context.Context, stdin string, args ...string) securityResult

func runSecurity(ctx context.Context, stdin string, args ...string) securityResult {
	// The subcommand (args[0]) is traced; the item names are not.
	what := "set"
	if len(args) > 0 && args[0] != "-i" {
		what = args[0]
	}
	span := trace.Begin("keychain", what)
	cmd := exec.CommandContext(ctx, "security", args...)
	// Once the timeout has killed security(1), do not wait for whatever
	// it started (a password prompt) to let go of its output: Run would
	// otherwise block for as long as that lives.
	cmd.WaitDelay = time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		span.Fail("no answer in time (locked, or waiting on a prompt?)")
	} else {
		span.End("")
	}
	res := securityResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.Exited() {
		res.exit = exit.ExitCode()
	} else {
		res.err = err
	}
	return res
}

// KeychainTimeout bounds one call to the system keychain, which may block
// on a prompt (SLACKCLI_KEYCHAIN_TIMEOUT_MS; default 3s).
func KeychainTimeout() time.Duration {
	return appinfo.EnvDurationMS("KEYCHAIN_TIMEOUT_MS", 3*time.Second, false)
}

// securityItemNotFound is security(1)'s exit status for errSecItemNotFound.
const securityItemNotFound = 44

func (k macKeychain) Get(service, account string) (string, error) {
	timeout := KeychainTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res := k.run(ctx, "", "find-generic-password", "-s", service, "-a", account, "-w")
	return keychainGetResult(res, ctx.Err() != nil, timeout)
}

// keychainGetResult interprets the outcome of `security
// find-generic-password`: only its "item not found" status means that the
// secret is absent. Its stderr is not quoted: the exit status is enough.
func keychainGetResult(res securityResult, timedOut bool, timeout time.Duration) (string, error) {
	switch {
	case timedOut:
		return "", fmt.Errorf("the keychain did not answer within %s (a prompt may be waiting for you)", timeout)
	case res.err != nil:
		return "", fmt.Errorf("the keychain could not be read: %w", res.err)
	case res.exit == securityItemNotFound:
		return "", nil
	case res.exit != 0:
		return "", fmt.Errorf("the keychain could not be read (security exited with status %d)", res.exit)
	}
	return strings.TrimSpace(string(res.stdout)), nil
}

// Set feeds the command to `security -i` on stdin so that the secret never
// appears in the process list (argv is world-readable via ps).
func (k macKeychain) Set(service, account, value string) error {
	command, err := addPasswordCommand(service, account, value)
	if err != nil {
		return err
	}
	timeout := KeychainTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res := k.run(ctx, command, "-i")
	return keychainSetResult(res, ctx.Err() != nil, timeout)
}

// securityReturned matches the line with which `security -i` reports the
// status of a sub-command that failed ("add-generic-password: returned 36").
var securityReturned = regexp.MustCompile(`(?m)^add-generic-password: returned (-?\d+)\s*$`)

// keychainSetResult interprets the outcome of an add-generic-password fed
// to `security -i`, which reports a sub-command that failed on stderr (its
// exit status alone is not a reliable sign of it).
//
// The error carries a status and nothing else of what security(1) wrote:
// its messages can quote the command it was given, which holds the secret.
func keychainSetResult(res securityResult, timedOut bool, timeout time.Duration) error {
	switch {
	case timedOut:
		return fmt.Errorf("the keychain did not answer within %s (a prompt may be waiting for you)", timeout)
	case res.err != nil:
		return fmt.Errorf("the keychain could not be written: %w", res.err)
	case res.exit == 0 && len(bytes.TrimSpace(res.stderr)) == 0:
		return nil
	}
	if m := securityReturned.FindSubmatch(res.stderr); m != nil {
		return fmt.Errorf("the keychain did not take the secret (security add-generic-password returned status %s)", m[1])
	}
	return fmt.Errorf("the keychain did not take the secret (security exited with status %d and reported an error)", res.exit)
}

func (k macKeychain) Delete(service, account string) {
	ctx, cancel := context.WithTimeout(context.Background(), KeychainTimeout())
	defer cancel()
	k.run(ctx, "", "delete-generic-password", "-s", service, "-a", account)
}

// maxSecurityCommand is the longest command line given to `security -i`.
// It reads lines into a 4096-byte buffer and takes what does not fit as the
// next command, so a longer line would let the end of a secret be run as a
// command of its own.
const maxSecurityCommand = 4000

// addPasswordCommand returns the line that makes `security -i` store value.
// `security -i` runs one command per line, so a value that cannot be kept
// on one line of a safe length is refused rather than passed on: it could
// end the command and start another one. The errors do not quote the
// values.
func addPasswordCommand(service, account, value string) (string, error) {
	for _, arg := range []struct{ what, v string }{{"service name", service}, {"account name", account}, {"secret", value}} {
		if strings.ContainsAny(arg.v, "\n\r\x00") {
			return "", fmt.Errorf("the %s contains a line break or a NUL byte, which the keychain tool (security) cannot be given", arg.what)
		}
	}
	command := "add-generic-password -U -s " + securityQuote(service) +
		" -a " + securityQuote(account) + " -w " + securityQuote(value) + "\n"
	if len(command) > maxSecurityCommand {
		return "", fmt.Errorf("the secret is too long for the keychain tool (security): its command would be %d bytes long, the limit is %d", len(command), maxSecurityCommand)
	}
	return command, nil
}

// securityQuote quotes an argument for the parser of `security -i`, which
// splits a line into words like a shell without expansions: a double-quoted
// word ends at the next double quote, and a backslash makes the character
// that follows it literal. Escaping backslashes and double quotes is
// therefore enough for everything that stays on the line (spaces, single
// quotes, leading dashes, non-ASCII bytes); what does not stay on it is
// refused by addPasswordCommand.
func securityQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}
