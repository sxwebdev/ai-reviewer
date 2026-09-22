<p align="center">
  <img src="assets/promo.webp" alt="ai-reviewer — a self-hosted GitLab review agent for engineering teams" width="100%">
</p>

# ai-reviewer

**A self-hosted GitLab review agent for engineering teams.** It watches the open
merge requests of the repositories you configure, reviews each one with Claude
Code when its head commit changes, posts the findings that survive validation as
inline discussions, and sends every team a Slack digest twice a day. Optionally,
Linear workflow state prevents already-approved work from nagging every remaining
reviewer and identifies approved MRs whose cards are still stuck in `In Review`.

It plugs into GitLab and Slack and changes nothing about how you already work —
no CI job to add, no bot to invite into a workflow, nothing for reviewers to
learn.

## Why

Review queues rot quietly. An MR waits three days because nobody noticed it was
assigned to them; a pipeline went red on Friday and nobody looked until Monday;
a thread from last week is still unresolved and the author thinks it is done.
None of that is a hard problem, but somebody has to keep asking.

ai-reviewer is that somebody:

- **A first pass before a human spends attention.** Several specialist passes,
  then a round that argues against their own findings, then everything unproven
  is thrown away — so what reaches the merge request is worth reading, and lands
  only on lines it actually changed.
- **A digest that names people, not counts.** Twice a day per team, in Slack,
  with real `@`-mentions: who owes a review, whose MR has unresolved threads,
  merge conflicts, or a failed pipeline. Not a dashboard nobody opens.
- **Safe by construction.** It never approves, merges, or touches anyone else's
  comments; Claude gets a read-only checkout and cannot reach GitLab at all.
  Both write paths ship **off** — it observes and records until you switch them on.

## What it never does

Hard invariants, not settings:

- never approves or merges a merge request;
- never resolves or deletes anyone else's threads or comments;
- never changes reviewers, labels, titles or descriptions;
- never lets the model write to a repository, push, or talk to GitLab — it reads
  code and proposes findings, and nothing it proposes is posted unverified;
- never comments on code the merge request did not change;
- never logs a secret, and never sends one to GitLab, Slack or a metric.

## Install

You need a GitLab personal access token (`api` scope), a Slack bot token and a
Claude Code OAuth token. A Linear API key is optional for Linear-aware MR
classification and the `In Review` summary. PostgreSQL comes with the stack.

```bash
git clone https://github.com/sxwebdev/ai-reviewer && cd ai-reviewer

cp .env.example .env                  # your three tokens
cp config.example.yaml config.yaml    # your teams and their repositories

docker compose up -d
```

That is the whole install: it builds the image, starts PostgreSQL, creates the
schema and runs the service.

**Nothing is posted on the first run.** Both write paths ship switched off, so
the service watches and records until you turn them on — you can read what it
would have said before it says anything.

**→ [Full installation guide](docs/installation.md)** — GitLab and Slack setup,
Claude authentication, running without compose.

## Documentation

| Guide                                  | What is in it                                                     |
| -------------------------------------- | ----------------------------------------------------------------- |
| [Installation](docs/installation.md)   | database, GitLab/Slack/Claude setup, Docker                       |
| [Configuration](docs/configuration.md) | the config file, every environment variable, Vault                |
| [Operations](docs/operations.md)       | commands, dry-run switches, schedules, metrics, troubleshooting   |
| [Architecture](docs/architecture.md)   | how a review runs, the team model, where state lives, limitations |
| [Security model](docs/security.md)     | what the agent may touch, and why each boundary is where it is    |
| [Development](docs/development.md)     | building, testing, code generation                                |

## License

MIT.
