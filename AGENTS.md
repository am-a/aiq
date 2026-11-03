# Working on aiq

aiq routes pooled Claude Code, Codex and Antigravity accounts. It handles account
logins and reads session transcripts, so read [docs/quality.md](docs/quality.md)
before changing code. [README.md](README.md) describes the architecture.

- Every change, including a version bump, goes through a pull request.
- A pull request needs the [full gate](docs/quality.md#integrating-a-change), including
  an independent review of its latest commit.
- Every change that is not trivial follows the
  [manual review workflow](docs/quality.md#manual-review-workflow): independent design
  review and selected specialists before substantial code, then a fresh bug hunt,
  specialists again and a separate adjudicator. A review that did not happen stays
  incomplete, and the change does not merge.
- Keep tests away from real data: set `AIQ_DATA_DIR` to a temporary directory, and
  use invented values for accounts, addresses and secrets.
- This repository is public. Keep credentials, transcripts, handoff notes, private
  paths and employer or client details out of commits, comments and logs.
- Report what was run and what was not. A check that did not run has not passed.
