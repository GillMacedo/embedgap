# Contributing

Thanks for helping. Fork the repo, make your change on a branch, and open a pull request. Only the maintainer merges to `main`.

## Setup

    make test
    make lint

You need Go 1.22 or newer. `make lint` also needs Claude Code. The dev container in `.devcontainer/` sets up both.

## Pull requests
- PRs go through an automatic check before a person reads them: changes over 30 lines need a linked open issue (`Closes #<number>`), code changes need tests, `make test` must pass, and a PR can have at most 800 changed lines.
- Keep each one to a single change, with tests.
- Each finding needs a test for what it flags and for what must stay quiet.
- Each new kind of input needs a test that broken input exits 2.
- Update `ROADMAP.md` and docs when relevant.
- Releases are automatic: a merge that changes what users install becomes a new version with its changelog line taken from the PR title, so write a title that says what changed for users. Edit `CHANGELOG.md` yourself only if you want different wording. Docs and CI changes wait for the next one.

## Working with AI agents
Agents are welcome. `CLAUDE.md` and `AGENTS.md` have the project's instructions, and `.claude/commands/` has common tasks. Say in your PR that an agent helped, and review the result yourself before submitting. You're responsible for what you submit.

By contributing, you agree that your work is licensed under the MIT License.
