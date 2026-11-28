# New session: a project-first work context (design)

Status: proposal for review (SYR-H2N68). Nothing here is built yet; the follow-up
tickets come after the review.

## Problem

The New session dialog (`web/src/app/layout/session-dispatch-modal.component.ts`) asks for
a machine, a folder and an optional "New worktree branch". Opened from the Commander's
Terminal button, nothing says which project, ticket or repo that branch is for, so the
field is a guess. People think in projects and work items, not in folders and branches.

## Entry points and what each already knows

| Entry point | Where | Knows | Asks |
|---|---|---|---|
| Live work: Agent chat / Terminal | Command, left rail | nothing | everything, defaults from recent use |
| Commander tab `+` menu | Command, tabs | nothing | as above |
| Commander chat header: Terminal | Command, chat header | the chat's machines (scope) and folder | project (optional) |
| Palette: New agent chat… / New terminal… | everywhere (⌘K) | the current view: a selected project or ticket on Work, a machine on Fleet | what the view doesn't say |
| Work: New task | Work, header | the board's project filter (if one) | work item, repo |
| Ticket panel: Work on it (new) | Work, ticket panel | project, ticket, its repo bindings | where (machine), agent |
| Projects and repos: Chat on a repo | Projects dialog | project, repo | work item (optional) |
| Machine detail: Terminal / New chat | Fleet, machine detail | the machine | project (optional) |
| Phone: inbox | Mobile | nothing (phone sessions are rare) | as Live work, one screen at a time |

The Wall shows sessions only and has no New session entry.

## The flow

One shared **work context** with five steps. A step is skipped when the entry point (or
the steps before it) already answers it, and every answer can be changed on the summary
line at the top.

1. **Project**: from the entry point; else the project of the selected ticket, the board's
   filter or the most recent session; else asked. "No project" is always an option and is
   what a plain shell uses.
2. **Work item**: an existing initiative, epic or ticket of the project (search by ref or
   title), or **New task** typed as one line (created as a ticket in the project when the
   session starts). Skipped with no project.
3. **Repo(s)**: the work item's repo bindings, else the project's repos. Pick one or
   several, or type a path, a forge URL or `ssh://host/dir`. Skipped with no project.
4. **Branch / worktree**: shown only once a repo is known. Default derived from the work
   item: `<type>/<ticket-ref>` (e.g. `feat/syr-h2n68-work-context`); for a new task, a
   slug of its line. "Work in the checkout (no worktree)" is a toggle.
5. **Where and what**: machine (the scorer's suggestion first: checkout locality, headroom,
   machine policy), then Agent (account, model) or Terminal (shell).

**Plain shell stays one click.** Terminal from Live work, the palette or a machine opens a
shell in the home folder at once, with no project. The dialog only opens when the user
holds ⌥ (or picks "Terminal in a project…" from the menu next to the button).

## Mockups

Desktop, opened from a ticket (project, ticket and repo known):

```
┌ New session ─────────────────────────────────────────────────────────┐
│ Syre › SYR-H2N68 Project-first New session flow › syre       [edit]  │
│                                                                      │
│ Branch   [ feat/syr-h2n68-work-context          ]  ☐ no worktree     │
│ Where    [ bocabox ▾ ]  checkout here · 18 GB free                   │
│                                                                      │
│ (•) Agent   [ Claude 2 ▾ ]  [ Opus · high ▾ ]                         │
│ ( ) Terminal                                                         │
│                                                                      │
│ Prompt  [ Implement the work context component…                  ]   │
│                                             [ Cancel ]  [ Start ▸ ]  │
└──────────────────────────────────────────────────────────────────────┘
```

Desktop, opened from Live work with ⌥ (nothing known):

```
┌ New session ─────────────────────────────────────────────────────────┐
│ Project   [ Syre ▾ ]           (recent: Syre, jk, Left City, none)    │
│ Work      [ Search SYR-… or type a new task                    ]     │
│           ↳ New task: "Fix the flaky proctree test"                  │
│ Repo      ( ) syre  ( ) syre-web  [ path, URL or ssh://… ]           │
│ Branch    (appears once a repo is chosen)                            │
│ Where     [ bocamacmini ▾ ]                                          │
│ (•) Agent  ( ) Terminal                                              │
│                                             [ Cancel ]  [ Start ▸ ]  │
└──────────────────────────────────────────────────────────────────────┘
```

Phone, one step per screen, a summary line that grows:

```
┌──────────────────────┐   ┌──────────────────────┐   ┌──────────────────────┐
│ New session          │   │ Syre                 │   │ Syre › SYR-H2N68     │
│ Project              │   │ Work item            │   │ Branch               │
│ ▸ Syre               │   │ [ search / new task ]│   │ [ feat/syr-h2n68-…  ]│
│ ▸ jk                 │   │ ▸ SYR-H2N68 Project… │   │ Where [ bocabox ▾ ]  │
│ ▸ No project (shell) │   │ ▸ New task…          │   │ [ Agent ] [ Terminal]│
│                      │   │                      │   │ [      Start ▸      ]│
└──────────────────────┘   └──────────────────────┘   └──────────────────────┘
```

## Data and component

- `WorkContext { projectId?, ticketId?, newTaskTitle?, repoIds[], paths[], branch?, worktree: boolean, workerId?, kind: 'chat' | 'terminal' }`.
- `SessionDispatchRequest` grows `context?: Partial<WorkContext>`, which each entry point
  fills with what it knows; `ModeUiStore.newSession(kind, context)`.
- `<sy-work-context>` replaces the branch field and the folder part of the context bar in
  the session dialog. Steps skip themselves from the context they receive.
- Starting: a new task is created first (ticket in the project, with the repo bindings),
  then the session is dispatched with `ticket_id`, `repo_id`, `branch` and
  `use_worktree`, so the work key, placement locality (SYR-WCH0N) and Run on (SYR-0M4QQ)
  all apply.

## Questions for review

1. Should "New task" create a ticket at once, or only when the session produces work?
2. Branch naming: `<type>/<ticket-ref>` (feat/bug/chore from the ticket kind), or the
   ticket ref alone?
3. Is ⌥-click (plus a menu entry) the right escape from the one-click shell, or should the
   one-click shell be the menu entry instead?

## Follow-up tickets (after review)

1. `<sy-work-context>` component with the five steps and skip rules (desktop).
2. Entry points pass their context (the table above), including a Work on it button in the
   ticket panel.
3. New task creates a ticket with repo bindings before dispatch.
4. Phone layout: one step per screen.
5. e2e for each entry point's prefill and the one-click shell.
