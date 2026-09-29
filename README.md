# brrrr

A small PR review TUI for gh-dash. All application code lives in `main.go`.
It uses go-gh's native API clients with your existing `gh` authentication and host
configuration (`GH_HOST` is respected). PR requests do not invoke the CLI.
The SDK may call `gh auth token` when credentials are held in the system keyring.
No tokens or comments are stored by this app.

Build with Go 1.26.7 or newer and keep `gh` on your PATH:

```sh
go build -o brrrr .
./brrrr --repo owner/repo --pr 123
```

Put `brrrr` on your PATH, then add this to your gh-dash configuration:

```yaml
keybindings:
  prs:
    - key: R
      name: review + approve
      command: brrrr --repo {{.RepoName}} --pr {{.PrNumber}}
```

| Key | Action |
| --- | --- |
| `j` / `k`, arrows | Move through diff and existing comments |
| Mouse wheel | Scroll three rows; horizontal wheel scrolls sideways |
| Left-click a diff row | Select the row |
| Right-click a code line | Open its local comment in the modal |
| Middle-click a code line | Remove its local comment |
| `[` / `]` | Previous / next file (including PR discussion) |
| `h` / `l`, left / right | Scroll horizontally |
| `pgup` / `pgdown`, `ctrl+u` / `ctrl+d` | Move half a screen |
| `g` / `G` | First / last line |
| `c` | Open a centred modal to add or edit the selected line's local comment |
| `enter` / `esc` | Save / cancel in the multiline comment editor |
| `shift+enter` | Insert a newline in the editor (`ctrl+j` is a terminal fallback) |
| `/`, then `enter` | Run a regex search across diff text, file headings and existing comments |
| `n` / `p` | Next / previous matching line, wrapping at either end |
| `x` | Remove the selected line's local comment |
| `r` | Refresh PR, diff and existing comments |
| `a` | Approve: check the head, submit all local comments with `APPROVE`, exit |
| `s` | Submit: check the head, submit local comments with `COMMENT` without approving, exit |
| `q` | Quit; confirm discarding local comments if any |

Additions and context lines use `RIGHT`; deletions use `LEFT`. A filled marker
indicates a local comment, and a hollow marker indicates existing GitHub comments.
Existing comments are read-only. Inline comments appear after their code line;
general PR comments, review summaries and outdated/unmatched comments appear in
the PR discussion section. Multiline comments occupy separate navigable rows;
use horizontal scrolling for long lines.

The comment modal keeps the diff visible behind it and resizes with the terminal.
Mouse scrolling is paused while editing so the comment stays on its original line.
The diff uses the Bubbles viewport for scrolling and fills the space between the
fixed header and footer. The modal uses a Bubbles textarea and Lip Gloss layers;
Bubble Tea handles terminal rendering and screen restoration.
The initial fetch shows a spinner until the PR is ready; failures show a retry
screen. Diff text and comment anchors are indexed in the background once per
load or refresh. The Bubbles viewport manages scrolling; the diff renderer clips
indexed text before styling only the visible rows. Long lines use Unicode-aware
checkpoints, so scrolling horizontally does not rescan their entire contents.
Editor key hints appear only inside the modal.

Search runs only after Enter and caches matching lines. It uses Go regular
expressions, is case-insensitive by default (`(?-i)` enables case-sensitive matching),
and excludes the diff's `+`/`-`/context prefix from code-line searches. Each matching
line is a stop for `n`/`p`; the first match on a long line is scrolled into view.
Matching lines are underlined. Escape clears the current search while browsing,
entering a pattern or searching; in the comment editor it cancels the edit.
Enter with an empty pattern also clears the search. Refresh clears old search results.

The comment editor uses a consistent neutral grey background with explicit text,
placeholder and cursor colours. The selected diff row is highlighted without an
arrow in the gutter. Terminals that cannot distinguish Shift+Enter from Enter
can use `ctrl+j` for a newline. `ctrl+s` remains an alternative save shortcut.

Refresh retains local comments with matching file, side and line text, even when
line numbers move. Repeated text must have a unique match using surrounding
lines. Changed, missing, renamed or ambiguous anchors are discarded; the status
reports how many were kept and discarded. A failed refresh leaves your current
diff and local comments intact. Cancelling quit during a refresh returns to the
pending refresh; submission stays locked until it finishes.

Approval works without comments. Comments-only submission requires at least one
local inline comment and works when reviewing your own PR. It sends the fixed
review body “Inline review comments.” required by the GitHub API; there is no
summary editor. Clicking existing GitHub comments does not edit or delete them.

The app checks the head and base before and
after loading the diff, and again immediately before posting one review with
`event: APPROVE` or `event: COMMENT` and the reviewed `commit_id`. A changed PR requires a refresh.
GitHub does not offer an atomic “approve only if head is still this SHA” operation
on this endpoint, so a push between the last check and the POST remains possible;
the review is explicitly tied to the SHA you reviewed.

During submission, input is locked to avoid duplicate requests. If a POST fails,
check GitHub before retrying: the server may have received it even if the response
was lost. Press `ctrl+r` to acknowledge this, then `a` or `s` to retry. Refresh does not clear
this retry guard. There are no pending reviews, replies, resolution controls,
request-changes mode, summary editor or persistence.

```sh
go test ./...
go vet ./...
go test -run '^$' -bench BenchmarkLargeDiffScroll -benchmem .
go test -run '^$' -bench 'Benchmark(LongLineScroll|RepeatedAnchorRefresh|LoadCompletion)$' -benchmem .
```

Tests use an in-memory HTTP transport through the real go-gh SDK and never submit
to GitHub.

## TODO

* Support multiple line selection
* Add suggestions (will need multiple lines for the most part) via keybind in edit view
