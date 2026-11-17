# Contributing to slackcli

Thanks for contributing. The conventions of the codebase are in [AGENTS.md](AGENTS.md); this file covers the workflow.

## Requirements

- Go, at the version given by the `go` line of `go.mod` (`actions/setup-go` reads the same file in CI).
- `make`. Optional: `goreleaser` for `make snapshot`; `staticcheck` and `golangci-lint` for `make lint-extra`.

## Make targets

```bash
make build        # static binary in build/slackcli (version "dev")
make install      # install into GOBIN
make test         # go test ./...
make lint         # go vet + gofmt check (what CI enforces)
make fmt          # gofmt -w cmd internal .github
make lint-extra   # staticcheck / golangci-lint, if installed
make snapshot     # every release archive into dist/, without publishing
make clean
make              # lint, test, build
```

`make test PKGS=./internal/update/...` narrows build, vet and test to some packages.

## Tests

Tests never talk to Slack and never touch real credentials: fake the API with `slackapi.NewFake(handler)`, raw HTTP with `net/http/httptest`, and use `t.TempDir()` / `t.Setenv()`. A test that reads the home, config or cache directory calls `testenv.Isolate(t)` (`internal/testenv`) first. Tests run on Windows too: do not assert bare permission bits, and build expected paths with `filepath`. Only the standard `testing` package is used.

The credential-extraction test against the applications installed on your own machine is skipped unless `SLACKCLI_LIVE_EXTRACT=1` is set.

CI (`.github/workflows/ci.yml`) builds, vets and tests on Linux, macOS and Windows, and additionally checks `gofmt`, `go mod tidy -diff`, `go test -race ./...`, `shellcheck` on `install.sh` and `scripts/install-skill.sh`, and `goreleaser check`. It also builds a snapshot release (`make snapshot`, which cross-compiles every release target) and runs `go run ./.github/scripts/checkdist dist` to check that the archives are the ones `install.sh` and `slackcli update` download.

## Adding a command

1. Put the Slack logic in `internal/slack/<domain>`: functions taking `ctx` and a `*slackapi.Client`, returning structs with `json:"snake_case"` tags. Test it with `slackapi.NewFake`.
2. Create `internal/cmd/<name>cmd` exporting `func New(rt *clictx.Context) *cobra.Command` (or add a subcommand to an existing package). In `RunE`: validate flags, get the client with `rt.Client(ctx, rt.EffectiveWorkspace(flag))` inside `clictx.WithAutoRefresh`, `return rt.Print(result)`, and return errors instead of printing them. `internal/cmd/unreadscmd` is a small example. Use the helpers of `internal/cmd/cmdutil` rather than writing your own: for a command taking a `<target>` (URL, `#channel`, ID), `cmdutil.ParseTarget` (or `ParseChannel`, `ParseMessageTarget`) with `cmdutil.AddTargetWorkspaceFlag`, then `cmdutil.WithTarget`, which gets the client and resolves the conversation with auto-refresh (`internal/cmd/channelcmd/mark.go` is a small example); `cmdutil.ParseTS`, `CheckLimit`, `CheckOptionalLimit` and `CheckMaxChars` to validate flags; `cmdutil.PrintList` for a result holding a list, so that an empty one is printed as `[]`; `cmdutil.AllowDashText` when a free-text argument may start with a dash.
3. Register a new top-level command in `internal/cmd/root/root.go`.
4. Write the `Short`/`Long` help and the flag usage carefully: arguments, the JSON printed, side effects, and whether safe mode covers it. A command that posts, edits or deletes must honour `rt.SafeMode()`.
5. Update `README.md` (command map, safe mode table, environment variables) and `skills/slackcli/` if the command has non-obvious workflow, safety, targeting or output behaviour.
6. Run `make lint test build` and read your command's `--help` in the built binary.

## Releasing (maintainers)

Releases are binaries published on GitHub Releases by GoReleaser.

```bash
git tag -a v1.2.3 -m "v1.2.3"
git push origin v1.2.3
```

Pushing a tag matching `v[0-9]+.[0-9]+.[0-9]+*` triggers `.github/workflows/release.yml`, which vets, tests and runs `goreleaser release --clean` (`.goreleaser.yaml`): it builds `slackcli_<darwin|linux|windows>_<amd64|arm64>.tar.gz` (`.zip` on Windows) and `checksums.txt`, stamps the version into the binary and publishes the GitHub release. A tag with a pre-release suffix such as `v1.2.3-rc.1` is published as a pre-release, which `install.sh` and `slackcli update` never pick up.

The archive names, the binary's place at the archive root and `checksums.txt` are a contract between `.goreleaser.yaml`, `install.sh` and `internal/update`; a test in `internal/update` fails when they drift. Try a release locally with `make snapshot`.

## Renaming the tool

The name is defined in `internal/appinfo/appinfo.go` (`Name`, `EnvPrefix`, `Repo`), the module path in `go.mod`, the directory `cmd/slackcli`, two lines of `.goreleaser.yaml`, three lines at the top of `install.sh`, and the directory `skills/slackcli`. The Markdown files always write the name literally (`slackcli`, `SLACKCLI_`, `maelvls/slackcli`), so a search-and-replace covers them.
