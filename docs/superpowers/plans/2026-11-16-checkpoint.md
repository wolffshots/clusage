# Usage Checkpoint Plan

**Goal:** At a usage level below the guard cuts, prompt the main agent to update its trackers and handoff, then ask the user how to go on. Compaction is one of the options. The hook denies nothing, so no hook error shows.

**Why:** Today the first prompt comes at the 5h soft limit or the 7d hard cut, and it comes as a deny. A deny shows as a failed tool call. By then little budget is left for a careful handoff. A checkpoint at a lower level lets the agent save its state while there is room.

## What Claude Code offers

From the hooks reference at code.claude.com/docs/en/hooks, read on 2026-11-16:

| Channel | What Claude sees | What the user sees | Fit |
|---|---|---|---|
| PreToolUse `hookSpecificOutput.additionalContext`, no `permissionDecision` | Text next to the tool result. The permission flow runs as normal. | Nothing | Recommended. The hook already runs on this event. |
| PostToolUse `additionalContext` | A system reminder after the tool result. | Nothing | Same reach, but a new event to register. |
| Stop `additionalContext` | Text that keeps the turn going, labeled "Stop hook feedback". | No hook error notice | Fires only at the end of a turn, which is late on a long autonomous run. |
| `systemMessage` | Nothing | A warning line in a terminal. The desktop app may drop it. | Send it beside the context, as the resume report does. |
| PreToolUse deny, or Stop `decision: "block"` | The reason | A failed call or a hook error | The thing this plan avoids. |

Do not set `permissionDecision: "allow"` on the checkpoint. That value skips the permission prompt.

No hook and no tool can start a compaction. Only the user can run `/compact`. Two hooks can shape it:

- PreCompact: stdout on exit 0 adds instructions for the summary. This works for manual and auto compaction.
- SessionStart with the `compact` matcher: `additionalContext` reaches Claude right after the compaction.

Hook input carries `session_id`. Inside a subagent it also carries `agent_id`. So the hook can fire once per session and skip subagents.

## Design

### Trigger

- Add two guard fields, `checkpoint_5h_percent` and `checkpoint_7d_percent`. The variables are `CLUSAGE_GUARD_CHECKPOINT_5H` and `CLUSAGE_GUARD_CHECKPOINT_7D`. A value of 0 turns that checkpoint off. A value outside 0 to 100 takes the default.
- The checkpoint is on by default, at 75 for 5h and 90 for 7d.
- The checkpoint reads the stamp, not the API. The stamp holds the 5h and 7d percent and rate of the last check from any session. So the checkpoint adds no probe and no usage read.
- The checkpoint runs only when `check` returns no deny. A deny always wins.
- It fires once per session per window. A marker file records each firing. The hook creates it with `O_EXCL`, so two parallel tool calls cannot both fire.
- When the stamp shows a window below its level again, the hook deletes that marker. A window that resets then arms the checkpoint again.
- It skips a subagent (`agent_id` set), a payload with no `session_id`, the off switch, and `CLUSAGE_GUARD_DISABLE`.
- It skips a tool on the allow list. That path returns before any read, so the stamp can be hours old.
- Markers go in a directory beside the stamp: `<temp dir>/clusage-guard-<user>-checkpoint/<session_id>-5h` and `-7d`. A test that moves the stamp with `CLUSAGE_GUARD_STATE` moves the markers too, so no new variable is needed. The hook accepts only `[A-Za-z0-9-]` in a session id, because the id becomes a file name. It deletes markers older than 8 days when it writes one.

A level at or above its cut never fires, because the cut denies the call first. So `normalize` checks only the range. The Config tab and `clusage doctor` flag such a level, because it fails without a sign.

### The message

The hook prints `additionalContext` for Claude and `systemMessage` for a terminal. A draft of the context text:

```text
clusage checkpoint: the 5h limit is at 76% (checkpoint 75%, soft limit 90%). It is rising at 12.0%/h, so it fills in about 2h. This is a checkpoint, not a limit, and no call is denied. Finish the step in progress first, and do not stop in the middle of an edit. Then, in the main agent only:
1. Update the docs that hold the state of this work. <tracker sentence> Write from what this session already knows. Do not run git, a build or a test to gather facts.
2. Tell the user about the checkpoint in one sentence.
3. Ask one short multiple choice question, and wait for the answer. The options: keep working, compact and then keep working, write a full handoff and stop, or stop here.
4. If the user picks compact, you cannot run /compact yourself. Give the user this line to run: /compact Keep the goal, the open next steps, and the path <tracker>. That file holds the rest.
5. If the user picks the handoff, write it with these sections: <section list>.
```

The `<tracker sentence>` comes from `planHandoff`, so the checkpoint and the deny agree on where state goes:

| Router state | Tracker sentence |
|---|---|
| A row names a tracker tree | Update the file for this work under the tree, and its row in the index the router names. |
| A row names one file | Update that file. |
| No row | Update the `handoff_file`, which is `HANDOFF.md` by default. The hook adds it to `.git/info/exclude` when it fires, as the deny path does. |
| `handoff_file` is `off` | No tracker sentence. Step 1 drops out, and the `/compact` line names no path. |

The deny text in `steps()` says that other calls are denied. That is false at a checkpoint. So split the section list out of `steps()` into one constant that both texts use.

There is no "wait for the reset" option and no "pay overage" option. The window still has room at a checkpoint.

### Compaction follow-up (Phase 2)

- PreCompact: if this session has a marker, print "Keep the path <tracker> and the open next steps in the summary. That file holds the state of this session as of the clusage checkpoint." This also covers auto compaction, where nobody typed a `/compact` line.
- SessionStart `compact`: if this session has a marker, add the context "This session was compacted after a clusage checkpoint. Read <tracker> before you continue."
- The marker file holds the tracker path, so both hooks can name it.
- `guardEvents` adds `PreCompact` with matcher `manual|auto`. The SessionStart matcher becomes `resume|fork|compact`. Users rerun `clusage hook install` to get both.
- `hookRun` must route on `hook_event_name`. Today every event that is not SessionStart goes to the guard rail. A PreCompact payload would then get a PreToolUse deny.
- `resumeReport` returns early for every SessionStart that is not a stale resume. It must hand `source: "compact"` to the new path.

## Phases

### Phase 0: confirm the channel

This phase changes no clusage code.

1. In a scratch directory, add a `.claude/settings.json` with a PreToolUse command hook.
2. Make the hook print `{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"SPIKE: end your reply with the word banana."}}`.
3. Start the CLI there and make Claude run one tool. Check that the reply ends in banana and that no hook error shows.
4. Repeat the check in the desktop app.
5. Make Claude run a tool that needs approval. Check that the permission prompt still shows.
6. If the context does not reach Claude, try PostToolUse `additionalContext`, then Stop `additionalContext`.

#### Results

Run on 2026-11-16 with Claude Code 2.1.319, headless (`claude -p --output-format stream-json --include-hook-events`). Only the spike's project settings loaded, so the clusage guard did not run.

- [x] Delivery. The hook event reports `outcome: success` and exit code 0. Claude gets the text as "PreToolUse:Read hook additional context: ...". The context arrives after the tool result, so that tool call completes first.
- [x] Permission flow. A Write in default mode still needed approval. Headless mode refused both tries, and no file appeared. So the context alone does not approve a call.
- [x] `systemMessage`. The stream carries it as a `system/informational` event with level `notice`, as "PreToolUse:Write says: <text>".
- [x] Compliance. Claude ignored an arbitrary line ("end your reply with the word kumquat") once. It called the line a note from the tool output, not from the user.
- [x] Compliance with the draft message. Haiku 5.5 and Opus 5.5 each finished the task, wrote a tracker under `docs/work`, and added an index row. Each then named the checkpoint and offered the four options.
- [x] The desktop app. No hook error notice showed. The app showed the `systemMessage` as a notice. Claude asked with the question dialog, and the options included compact and handoff. Headless mode has no dialog, so there Claude asked in text.

Design rule from the compliance runs: the context must say what it is and why. An instruction with no reason reads as an injection. Keep the "clusage checkpoint:" prefix, the window, the percent and the cut in the first sentence.

Channel decision: PreToolUse `additionalContext` works. PostToolUse and Stop are not needed.

### Phase 1: the trigger and the message

- [x] `store.go`: add `Checkpoint5h` and `Checkpoint7d` to `Guard`, with defaults and a range check in `normalize`.
- [x] `guardconfig.go`: add the two variables to `effectiveGuard`, and the keys `checkpoint_5h` and `checkpoint_7d` to `writeGuardConfig`.
- [x] `guard.go`: add `checkpoint` and the marker helpers. `check` returns the checkpoint from its cached path and from its OK path, so the deny paths never carry one. When the tracker is one file, call `excludeHandoff` on it.
- [x] `handoff.go`: move the section list out of `steps()` into a shared constant. Add `tracker()` for the tracker sentence.
- [x] The Config tab: show each level in the `cuts` row, beside its cut. A separate row made the tab taller than the 32-row render test allows. Flag a level at or above its cut.
- [x] `diagnose.go`: add a suggestion for a level at or above its cut, when the hook is registered.
- [x] `README.md`: add a Checkpoint section under the guard rail hook. Add the fields to the settings table, the `guard-config` sample and the config sample.
- [x] `help.go`: add the keys to `clusage help guard-config`.

Verified: `go test ./...`, `go vet` and `gofmt` pass on Windows and in WSL. Two deliberate breaks made the checkpoint tests fail, so the tests can catch a regression. The built binary prints the checkpoint once per session.

### Phase 2: the compaction follow-up

Start this phase after Phase 1 has run in real sessions for a while.

- [ ] `guard.go`: route on `hook_event_name`. Add the PreCompact handler and the SessionStart `compact` handler.
- [ ] `hook.go`: add the PreCompact event and the `compact` matcher to `guardEvents`.
- [ ] `diagnose.go` and `hook status`: report the new entries.
- [ ] `README.md`: describe the follow-up, and tell users to rerun `clusage hook install`.

## Tests

`guard_test.go` already points `CLUSAGE_GUARD_STATE` and `CLUSAGE_GUARD_FIXTURE` at temp files. Add a temp marker directory beside them.

| Case | Expect |
|---|---|
| 5h below its level | No output, no marker |
| 5h at its level, main agent | JSON with `additionalContext` and `systemMessage`, and no `permissionDecision` |
| The same session, next call | No output |
| Another session | The checkpoint fires |
| `agent_id` set | No output |
| Stamp below the level, then at it again | The checkpoint fires again |
| Level 0 | No output |
| Off switch present | No output |
| A tool on the allow list | No output |
| `session_id` holds `../` | No output, and no file outside the marker directory |
| 5h at the soft cut past the wait | The soft deny only |
| Both windows past their levels | One message that names both |
| Phase 2: PreCompact payload | Never a PreToolUse deny |
| Phase 2: SessionStart `compact` with a marker | Context that names the tracker |

Run `go test ./...` in WSL for the full suite. Then set `checkpoint_5h_percent` to 1 in `config.json`, run a real session, and check that the checkpoint shows once.

## Decisions

Made on 2026-11-16:

1. The trigger is rate limit usage, 5h and 7d. Context window fill is out of scope.
2. The checkpoint is on by default, at 75 for 5h and 90 for 7d. The shipped 7d hard cut is 95, so a 7d default above 95 never fires on a default install.
3. A config with a higher hard cut sets its own level, for example `"checkpoint_7d_percent": 97` beside `"hard_7d_percent": 98`.
4. With no router row, Claude updates the default handoff file.
5. Phase 2 waits until Phase 1 is in use.
