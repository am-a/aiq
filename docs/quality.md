# Quality gates

aiq uses the shared [software-quality](https://github.com/orlenko/software-quality)
runner, pinned in `.quality/lock.json` and configured in `.quality/config.json`.

## Adoption status

| Stage | Status |
| --- | --- |
| Configured | Yes |
| Tool installed | On the maintainer's Mac only, outside this repository |
| Runner checks executed | No. Execution is off (`trusted_local: false`) |
| Review attested to the runner | No. Nobody can yet attest the required boundary |
| Verified adoption | No |

`doctor` therefore exits 2 (incomplete). That is the expected result, and it is not
a pass. CI and pull request review are the gates in force today.

## Integrating a change

1. Open a pull request. CI runs gofmt, `go vet` and `go test ./...` on Ubuntu and
   macOS.
2. Get the Codex review for the latest commit, by comment if needed. Read the
   finished state and any findings, and fix or answer each one.
3. Wait about five minutes before merging, then look again. A finding has arrived
   after a clean result before. The wait is a buffer and does not replace the review.
4. Merge with a merge commit, and tag a release on the merge commit.
5. Run `install-or-update.sh` on each machine, then confirm the version, the checkout
   commit and a daemon response.

## Checks and their effects

The runner configuration lists four checks. Execution stays off because no wrapper
contains these effects, and a temporary `HOME` is not containment.

| Check | Effects outside its snapshot |
| --- | --- |
| `gofmt` | None expected |
| `vet` | Writes the Go build cache; may download modules |
| `go-test` | The same, and queries the default tmux server. Tests avoid real aiq data by setting their own data directory, with no central guard |
| `installer-test` | Uses fake commands and temporary homes, and inherits the caller's environment and `PATH` |

`installer-test` is required by the runner configuration, but CI does not run it.

## Threat model

Assets and what protects them:

- **Account logins.** Each account's login stays private to its overlay home or the
  OS credential store. A login is a credential for a paid account.
- **Session transcripts.** They can hold anything a session saw. aiq reads them in
  place and does not redact them.
- **Handoff notes.** A note quotes prompts and tool output, and the next session
  reads it, so it can reach another account or vendor. Known secret formats are
  redacted, but anything else stays, such as a password in plain prose. Notes are
  owner-only, and archived notes are deleted after 30 days.
- **`state.db`.** It holds accounts, usage, leases and events, including worker
  prompts. It has no redaction.
- **Status page and API.** The daemon listens on loopback and rejects requests whose
  `Host` is not a loopback name. It has no login, so any local process can read it.

Outside the threat model: another process running as the same user, which can read
all of the above, and logs written by other tools.

## Running the runner

The launcher, trust module, core archive, cache and state must all live outside this
checkout, and a launcher inside it must never be used. Check each file against
`.quality/lock.json` and the approved digests before loading it.

```sh
python3 -I -S "$BOOTSTRAP/quality_launch.py" doctor \
  --repo "$TARGET" --base "$BASE" --head "$HEAD" --identity orlenko/aiq \
  --cache "$CACHE" --state "$STATE"
```

Use the same launcher's `install` command first. Do not pass `--execute-trusted`.

## Known gaps

- No recorded review attestation, so the required route stays incomplete.
- Codex's read-only boundary is unverified. One route with distinct identities
  required proves nothing about independence.
- The tests have no central guard against real data, and they can reach the default
  tmux server.
- `installer-test` is not in CI. No race run or secret, privacy or dependency scanner
  is configured.
- `main` has no branch protection, so the pull request rule is by practice.
- Tags are lightweight and unsigned, and `aiq version` prints no source revision.
- `state.db` schema upgrades ignore `ALTER TABLE` errors and have no migration test.
- The status page has no recorded accessibility check.
