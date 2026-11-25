# Pre-release walkthrough (real agent CLIs, no API key)

Syre's promise is that you chat with the agent CLIs you are already signed in to, with
no API key. The automated suites (Go tests and `web/e2e/subscription-chat.spec.ts`) cover
this against the deterministic mock engine. They cannot tell whether the real CLIs still
behave as our engines expect, so a person runs this checklist by hand on a real machine
**before every release tag**. If any required step fails, the release does not ship.

Allow about 30 minutes. Keep prompts tiny: every one of them uses real subscription quota.

## 0. Setup

- [ ] Use a real machine where the agent CLIs are installed and signed in the normal way
      (`claude`, `codex`, `grok`, `agy`, `cursor-agent`, `copilot`, plus any extra accounts
      such as `claude2` configured under `accounts:` in `syred.yaml`). Note which ones
      are installed; every installed one is tested below.
- [ ] No API key anywhere. In the shells that start `syre` and `syred`:
      `env | grep -iE 'api_key|SYRE_LLM_|ANTHROPIC_|OPENAI_|GEMINI_'` prints nothing, and
      Settings in the UI shows no commander API key (`GET /api/settings` reports
      `"llm_api_key_set": false`).
- [ ] Build the release candidate: `make web && make build`, from the commit you will tag.
- [ ] Start the hub and a worker on the same machine, **without** `--enable-mock-driver`:

      ```sh
      bin/syre serve --listen 127.0.0.1:8420 --db /var/tmp/syre-walkthrough/syre.db
      ```

      Sign in, open **Add machine → Manual** and run the command it shows with
      `--foreground` (for example `bin/syred join http://desk:8420 ABCD-EFGH --foreground`).

      (Or use the hub and `syred install` service you already run, upgraded to the
      candidate.)
- [ ] `bin/syred doctor` lists the installed CLIs, and nothing about it mentions a key.
- [ ] Sign in to the PWA with the `Sign in:` link the hub printed (no token typed), and
      sign in a phone with **Settings → Devices → Add a device** (scan the QR code). Both are
      listed in Settings → Devices; revoking the phone signs it out at once.
- [ ] Exactly one of these is true for each installed CLI: its account
      chip shows it signed in with usage (or "usage not available"), or it shows
      **Re-login**.

## 1. Chat with every installed CLI

For each installed, signed-in account (claude, claude2, codex, grok, agy, cursor, copilot):

- [ ] **New chat** → pick the account in the account chip → send `Reply with the word pong.`
- [ ] The reply streams in (text appears word by word, not all at the end), the status pill
      goes **Working** → **Done**, and nothing asks for an API key.
- [ ] Tool cards: send `List the files in the current directory.` and see a tool card
      (title, live status, expandable output).

| Account | Installed | Reply streamed | Tool card | Notes |
|---|---|---|---|---|
| claude | | | | |
| claude2 | | | | |
| codex | | | | |
| grok | | | | |
| agy | | | | |
| cursor | | | | |
| copilot | | | | |

## 2. Multi-turn context

- [ ] In one chat (claude, and one ACP agent such as grok or copilot): send
      `Remember the number 4817.`, then `What number did I ask you to remember?` The second
      reply says 4817.
- [ ] Reload the page and send a third turn (`And plus one?`). The reply uses the same
      context (4818): the chat resumed the CLI's own session.

## 3. Model, effort and approval selection

- [ ] The model popover lists the agent's real models (not a placeholder list), with
      descriptions, and the effort row shows only the levels the selected model supports
      (it changes or disappears when you switch models).
- [ ] Pick a non-default model and effort, send `Which model are you?`, and the chip and the
      reply agree.
- [ ] The approval popover lists the agent's own modes (for claude: plan, default/ask,
      accept edits, auto/bypass...). Choosing one updates the chip. Changing it mid-chat
      applies live or from the next turn, as the note says.

## 4. Permission round-trip

- [ ] With claude in the default ("Ask") approval mode, send
      `Create a file named syre-walkthrough.txt containing hi.`
- [ ] A **Permission needed** card appears with the tool call, the status pill shows
      **Needs you**.
- [ ] Click **Allow once**: the tool runs, the file exists, the turn ends **Done**.
- [ ] Repeat with **Reject** (ask it to delete the file): the agent reports it was not
      allowed, and the file is still there. Remove it yourself afterwards.

## 5. Asking the user (ask_user)

- [ ] Send `Before doing anything, use the ask_user tool to ask me whether I prefer tabs or
      spaces, then answer accordingly.`
- [ ] A question card appears with the options (and a text box if offered). Answer it; the
      same turn continues and uses your answer.

## 6. Stop

- [ ] Send `Count slowly from 1 to 200, one number per line.` and press **Stop** while it
      is working.
- [ ] Output stops within a couple of seconds, the pill shows **Cancelled**, and the next
      message in the same chat works.

## 7. Usage limits (only if an account is exhausted)

Skip unless one of your accounts is out of a usage window (the footer or account menu says
"Out of ... until ...").

- [ ] Sending on that account shows the pre-send warning with the window name and reset
      time, and offers **Use \<other account\>** when another account of the same agent is
      healthy.
- [ ] If you send anyway and it is rejected: the usage-limit card names the window (5-hour,
      weekly, ...) and the local reset time with a countdown, and the account chip shows
      **Out**.
- [ ] Switching to the offered account and sending again works.

## 8. Logged-out account: relogin flow

Use an account that is genuinely signed out (codex is often a good candidate), or sign one
out on purpose (for example a spare `claude2` profile).

- [ ] The account chip, the account menu and the footer show it as signed out / **Re-login**,
      and Send is blocked with "… is signed out".
- [ ] **How to sign in** (or the sign-in card) shows the exact relogin command, including the
      account's profile environment (for example `CLAUDE_CONFIG_DIR=~/.claude2 claude /login`).
- [ ] **Open terminal on \<host\>** opens a terminal tab on that machine that runs the command.
      Complete the sign-in there.
- [ ] **Refresh login and usage** (account menu or footer) turns the account healthy, and a
      chat on it works.

## 9. Commander fleet tools

- [ ] Start a **New chat** with **Fleet tools** on (claude is fine). Send
      `Use the Syre fleet tools to dispatch a shell task on <this machine> that runs
      "uname -a && date", wait for it, and tell me its output.`
- [ ] The transcript shows the fleet tool calls (`fleet_status`, `dispatch_task`,
      `get_task_result`, ...), the task appears in the sidebar and finishes, and the
      agent's reply quotes the real `uname -a` output.
- [ ] Open the task from the sidebar: its terminal output matches.

## 10. Footer usage

- [ ] The footer shows each account's binding usage window (percent and ring), or "out" /
      signed out.
- [ ] Its popover lists every account on every machine with per-window bars and reset
      times, and **Refresh** updates them. Numbers roughly match what the CLIs themselves
      report (`claude /usage`, `codex /status`, ...).

## Sign-off

Record the result in the release ticket or PR before tagging:

```
Walkthrough for vX.Y.Z on <machine>, <date>, by <name>
CLIs tested: claude, claude2, codex, ...   Skipped (not installed): ...
Sections 1-10: pass | fail (details) | skipped (reason)
```

A failure in sections 1-6, 8 (when a signed-out account is available), 9 or 10 blocks the
release. Section 7 is required only when an exhausted account is available.
