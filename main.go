package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/sourcegraph/go-diff/diff"
	"github.com/urfave/cli/v3"
)

type anchor struct {
	Path string `json:"path"`
	Side string `json:"side"`
	Line int    `json:"line"`
}

type inlineComment struct {
	anchor
	Body string `json:"body"`
}

type reviewEvent string

const (
	approvalEvent reviewEvent = "APPROVE"
	commentEvent  reviewEvent = "COMMENT"
)

type reviewRequest struct {
	CommitID string          `json:"commit_id"`
	Event    reviewEvent     `json:"event"`
	Body     string          `json:"body,omitempty"`
	Comments []inlineComment `json:"comments,omitempty"`
}

type pullRequest struct {
	Title string `json:"title"`
	State string `json:"state"`
	Head  struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
	} `json:"base"`
}

type githubComment struct {
	Path         string `json:"path"`
	Side         string `json:"side"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"original_line"`
	Body         string `json:"body"`
	State        string `json:"state"`
	User         struct {
		Login string `json:"login"`
	} `json:"user"`
}

type rowKind int

const (
	fileRow rowKind = iota
	hunkRow
	codeRow
	commentRow
)

type diffRow struct {
	Kind rowKind
	Text string
	anchor
	OldLine int
	NewLine int
}

type snapshot struct {
	PR   pullRequest
	Rows []diffRow
}

type githubPR struct {
	Repo   string
	Number int
	JSON   *api.RESTClient
	Diff   *api.RESTClient
}

var errHeadChanged = errors.New("PR changed; press r to refresh before submitting")

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	app := &cli.Command{
		Name: "brrrr", Usage: "Review a pull request and submit inline comments, with optional approval",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "repo", Usage: "Repository as owner/repo", Required: true},
			&cli.IntFlag{Name: "pr", Usage: "Pull request number", Required: true},
		},
		Action: run,
	}

	if err := app.Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd *cli.Command) (err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	repo := cmd.String("repo")
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(repo) || cmd.Int("pr") <= 0 || cmd.Args().Len() != 0 {
		return errors.New("use brrrr --repo owner/repo --pr <positive number>")
	}

	client, err := newGitHubPR(repo, cmd.Int("pr"), api.ClientOptions{})
	if err != nil {
		return
	}

	result, err := tea.NewProgram(newModel(ctx, client), tea.WithContext(ctx)).Run()
	if err != nil {
		return
	}

	if final, ok := result.(model); ok && final.submittedEvent != "" {
		if final.submittedEvent == approvalEvent {
			fmt.Fprintf(cmd.Writer, "Approved %s#%d with %d inline comment(s).\n", repo, client.Number, len(final.comments))
		} else {
			fmt.Fprintf(cmd.Writer, "Submitted %d inline comment(s) on %s#%d without approval.\n", len(final.comments), repo, client.Number)
		}
	}

	return
}

func (g githubPR) endpoint(suffix string) string {
	return fmt.Sprintf("repos/%s/pulls/%d%s", g.Repo, g.Number, suffix)
}

func newGitHubPR(repo string, number int, options api.ClientOptions) (client githubPR, err error) {
	options.Timeout = 30 * time.Second
	options.Log, options.LogIgnoreEnv = io.Discard, true
	options.EnableCache = false
	options.Headers = map[string]string{"Accept": "application/vnd.github+json"}
	jsonClient, err := api.NewRESTClient(options)
	if err != nil {
		err = fmt.Errorf("initialise GitHub client using gh authentication: %w", err)
		return
	}

	options.Headers = map[string]string{"Accept": "application/vnd.github.v3.diff"}
	diffClient, err := api.NewRESTClient(options)
	if err != nil {
		return
	}

	client = githubPR{Repo: repo, Number: number, JSON: jsonClient, Diff: diffClient}
	return
}

func (g githubPR) metadata(ctx context.Context) (pr pullRequest, err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	err = g.JSON.DoWithContext(ctx, http.MethodGet, g.endpoint(""), nil, &pr)
	if err == nil && (pr.Head.SHA == "" || pr.Base.SHA == "") {
		err = errors.New("GitHub returned missing PR commit information")
	}

	return
}

func (g githubPR) comments(ctx context.Context, endpoint string) (comments []githubComment, err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	for page := 1; ; page++ {
		if err = ctx.Err(); err != nil {
			return
		}

		var items []githubComment
		path := fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page)
		if err = g.JSON.DoWithContext(ctx, http.MethodGet, path, nil, &items); err != nil {
			return
		}

		comments = append(comments, items...)
		if len(items) < 100 {
			return
		}
	}
}

func (g githubPR) load(ctx context.Context) (result snapshot, err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	pr, err := g.metadata(ctx)
	if err != nil {
		return
	}

	response, err := g.Diff.RequestWithContext(ctx, http.MethodGet, g.endpoint(""), nil)
	if err != nil {
		return
	}

	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return
	}

	rows, err := parseDiff(data)
	if err != nil {
		return
	}

	var comments []githubComment
	for _, endpoint := range []string{g.endpoint("/comments"), fmt.Sprintf("repos/%s/issues/%d/comments", g.Repo, g.Number), g.endpoint("/reviews")} {
		page, subErr := g.comments(ctx, endpoint)
		if subErr != nil {
			err = subErr
			return
		}

		comments = append(comments, page...)
	}

	latest, err := g.metadata(ctx)
	if err != nil {
		return
	}

	if pr.Head.SHA != latest.Head.SHA || pr.Base.SHA != latest.Base.SHA {
		err = errHeadChanged
		return
	}

	result = snapshot{PR: latest, Rows: attachComments(rows, comments)}
	return
}

func (g githubPR) submitReview(ctx context.Context, pr pullRequest, request reviewRequest) (sent bool, err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	expectedState := "APPROVED"
	switch request.Event {
	case approvalEvent:
	case commentEvent:
		expectedState = "COMMENTED"
	default:
		return false, errors.New("review must use APPROVE or COMMENT")
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	data, err := json.Marshal(request)
	if err != nil {
		return
	}

	latest, err := g.metadata(ctx)
	if err != nil {
		return
	}

	if latest.Head.SHA != pr.Head.SHA || latest.Base.SHA != pr.Base.SHA {
		err = errHeadChanged
		return
	}

	if latest.State != "open" {
		err = errors.New("the PR is no longer open")
		return
	}

	var response struct {
		State string `json:"state"`
	}
	sent = true
	if err = g.JSON.DoWithContext(ctx, http.MethodPost, g.endpoint("/reviews"), bytes.NewReader(data), &response); err != nil {
		err = fmt.Errorf("review submission failed: %w", err)
		return
	}

	if response.State != expectedState {
		err = fmt.Errorf("GitHub returned review state %q instead of %s", response.State, expectedState)
	}

	return
}

func parseDiff(data []byte) (rows []diffRow, err error) {
	files, err := diff.ParseMultiFileDiff(data)
	if err != nil {
		return nil, fmt.Errorf("parse PR diff: %w", err)
	}

	for _, file := range files {
		path := strings.TrimPrefix(file.NewName, "b/")
		if file.NewName == "/dev/null" {
			path = strings.TrimPrefix(file.OrigName, "a/")
		}

		rows = append(rows, diffRow{Kind: fileRow, Text: path, anchor: anchor{Path: path}})
		for _, header := range file.Extended {
			if !strings.HasPrefix(header, "diff --git ") && !strings.HasPrefix(header, "index ") {
				rows = append(rows, diffRow{Kind: hunkRow, Text: header})
			}
		}

		for _, hunk := range file.Hunks {
			rows = append(rows, diffRow{Kind: hunkRow, Text: fmt.Sprintf("@@ -%d,%d +%d,%d @@ %s", hunk.OrigStartLine, hunk.OrigLines, hunk.NewStartLine, hunk.NewLines, hunk.Section)})
			oldLine, newLine := int(hunk.OrigStartLine), int(hunk.NewStartLine)
			for _, text := range strings.Split(strings.TrimSuffix(string(hunk.Body), "\n"), "\n") {
				if text == "" {
					continue
				}

				row := diffRow{Kind: codeRow, Text: text, anchor: anchor{Path: path}}
				switch text[0] {
				case '+':
					row.Side, row.Line, row.NewLine = "RIGHT", newLine, newLine
					newLine++
				case '-':
					row.Side, row.Line, row.OldLine = "LEFT", oldLine, oldLine
					oldLine++
				case ' ':
					row.Side, row.Line, row.OldLine, row.NewLine = "RIGHT", newLine, oldLine, newLine
					oldLine++
					newLine++
				case '\\':
					row.Kind = hunkRow
				default:
					return nil, fmt.Errorf("unsupported diff line in %s", path)
				}

				rows = append(rows, row)
			}

			if oldLine != int(hunk.OrigStartLine+hunk.OrigLines) || newLine != int(hunk.NewStartLine+hunk.NewLines) {
				return nil, fmt.Errorf("incomplete diff hunk in %s", path)
			}
		}
	}

	return
}

func commentRows(text string) (rows []diffRow) {
	for _, line := range strings.Split(text, "\n") {
		rows = append(rows, diffRow{Kind: commentRow, Text: line})
	}

	return
}

func attachComments(rows []diffRow, comments []githubComment) (result []diffRow) {
	locations := make(map[anchor]int)
	for i, row := range rows {
		if row.Kind == codeRow {
			locations[row.anchor] = i
			if row.OldLine > 0 && row.NewLine > 0 {
				locations[anchor{Path: row.Path, Side: "LEFT", Line: row.OldLine}] = i
			}
		}
	}

	attached := make(map[int][]diffRow)
	var discussion []diffRow
	for _, comment := range comments {
		if strings.TrimSpace(comment.Body) == "" || comment.State == "PENDING" {
			continue
		}

		text := fmt.Sprintf("@%s: %s", comment.User.Login, comment.Body)
		location := anchor{Path: comment.Path, Side: comment.Side, Line: comment.Line}
		if index, ok := locations[location]; ok && comment.Line > 0 {
			attached[index] = append(attached[index], commentRows(text)...)
			continue
		}

		if comment.Path != "" {
			label, line := "outside the displayed diff", comment.Line
			if line == 0 {
				label, line = "outdated or file comment", comment.OriginalLine
			}

			text = fmt.Sprintf("%s:%d %s (%s) — %s", comment.Path, line, comment.Side, label, text)
		}

		discussion = append(discussion, commentRows(text)...)
	}

	for i, row := range rows {
		result = append(result, row)
		result = append(result, attached[i]...)
	}

	if len(discussion) > 0 {
		result = append(result, diffRow{Kind: fileRow, Text: "PR discussion / comments outside this diff"})
		result = append(result, discussion...)
	}

	return
}

type fingerprint struct {
	Path string
	Side string
	Text string
}

func rowFingerprint(row diffRow) fingerprint {
	return fingerprint{Path: row.Path, Side: row.Side, Text: row.Text[1:]}
}

type adjacentLines struct {
	Text  [2]string
	Count int
}

type anchorContext struct {
	Before adjacentLines
	After  adjacentLines
}

type lineMatch struct {
	Index int
	Count int
}

type anchorMatches struct {
	lineMatch
	Contexts map[anchorContext]lineMatch
}

func indexAnchors(ctx context.Context, rows []diffRow, wanted map[fingerprint]bool) (lines map[fingerprint]*anchorMatches, surroundings map[int]anchorContext, err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	lines = make(map[fingerprint]*anchorMatches)
	surroundings = make(map[int]anchorContext)
	// Two directional passes find neighbours without rescanning repeated lines.
	for _, direction := range []int{1, -1} {
		nearby := make(map[struct{ Path, Side string }]adjacentLines)
		for offset := range rows {
			if err = ctx.Err(); err != nil {
				return
			}

			i := offset
			if direction < 0 {
				i = len(rows) - 1 - offset
			}

			row := rows[i]
			if row.Kind == fileRow || row.Kind == hunkRow {
				clear(nearby)
			}

			if row.Kind != codeRow {
				continue
			}

			key := rowFingerprint(row)
			side := struct{ Path, Side string }{row.Path, row.Side}
			neighbours := nearby[side]
			if wanted[key] {
				context := surroundings[i]
				if direction > 0 {
					context.Before = neighbours
				} else {
					context.After = neighbours
					matches := lines[key]
					if matches == nil {
						matches = &anchorMatches{Contexts: make(map[anchorContext]lineMatch)}
						lines[key] = matches
					}

					matches.Index, matches.Count = i, matches.Count+1
					match := matches.Contexts[context]
					match.Index, match.Count = i, match.Count+1
					matches.Contexts[context] = match
				}

				surroundings[i] = context
			}

			nearby[side] = adjacentLines{Text: [2]string{key.Text, neighbours.Text[0]}, Count: min(2, neighbours.Count+1)}
		}
	}

	return
}

func retainComments(ctx context.Context, previous, next snapshot, comments map[int]string) (retained map[int]string, err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	retained = make(map[int]string)
	if len(comments) == 0 {
		return
	}

	if previous.PR.Head.SHA == next.PR.Head.SHA && previous.PR.Base.SHA == next.PR.Base.SHA {
		byAnchor := make(map[anchor]int)
		for _, index := range slices.Sorted(maps.Keys(comments)) {
			byAnchor[previous.Rows[index].anchor] = index
		}

		for i, row := range next.Rows {
			if err = ctx.Err(); err != nil {
				return
			}

			if old, ok := byAnchor[row.anchor]; ok && row.Kind == codeRow && rowFingerprint(row) == rowFingerprint(previous.Rows[old]) {
				retained[i] = comments[old]
			}
		}

		return
	}

	wanted := make(map[fingerprint]bool)
	for _, index := range slices.Sorted(maps.Keys(comments)) {
		wanted[rowFingerprint(previous.Rows[index])] = true
	}

	oldLines, surroundings, err := indexAnchors(ctx, previous.Rows, wanted)
	if err != nil {
		return
	}

	newLines, _, err := indexAnchors(ctx, next.Rows, wanted)
	if err != nil {
		return
	}

	for _, index := range slices.Sorted(maps.Keys(comments)) {
		if err = ctx.Err(); err != nil {
			return
		}

		key := rowFingerprint(previous.Rows[index])
		old, next := oldLines[key], newLines[key]
		if next == nil {
			continue
		}

		if old.Count == 1 && next.Count == 1 && strings.TrimSpace(key.Text) != "" {
			retained[next.Index] = comments[index]
			continue
		}

		context := surroundings[index]
		if context.Before.Count == 0 && context.After.Count == 0 {
			continue
		}

		if match := next.Contexts[context]; old.Contexts[context].Count == 1 && match.Count == 1 {
			retained[match.Index] = comments[index]
		}
	}

	return
}

type activity int

const (
	browsing activity = iota
	loading
	editing
	submitting
	confirmQuit
	enteringSearch
	findingSearch
)

const (
	modalBackground = "#262626"
	modalForeground = "#eeeeee"
	modalMuted      = "#b8b8b8"
	modalAccent     = "#dedede"
)

type searchMatch struct {
	Row    int
	Column int
}

type searchResultsMsg struct {
	ID      uint64
	Pattern string
	Matches []searchMatch
	Err     error
}

type loadedMsg struct {
	ID       uint64
	Prepared preparedSnapshot
	Comments map[int]string
	Err      error
}

type submittedMsg struct {
	Sent  bool
	Err   error
	Event reviewEvent
}

type model struct {
	ctx            context.Context
	client         githubPR
	snapshot       snapshot
	lines          []displayLine
	loadID         uint64
	comments       map[int]string
	editor         textarea.Model
	searchInput    textinput.Model
	searchPattern  string
	searchID       uint64
	matches        []searchMatch
	viewport       viewport.Model
	spinner        spinner.Model
	files          []int
	activity       activity
	quitFrom       activity
	quitNotice     string
	selected       int
	width          int
	height         int
	notice         string
	blocked        bool
	submissionErr  bool
	submittedEvent reviewEvent
}

func newModel(ctx context.Context, client githubPR) model {
	editor := textarea.New()
	editor.Placeholder = "Write an inline comment…"
	editor.CharLimit = 65536
	editor.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "ctrl+j"))
	editor.SetVirtualCursor(true)
	base := lipgloss.NewStyle().Foreground(lipgloss.Color(modalForeground)).Background(lipgloss.Color(modalBackground))
	muted := base.Foreground(lipgloss.Color(modalMuted))
	state := textarea.StyleState{
		Base: base, Text: base, Placeholder: muted, CursorLine: base,
		LineNumber: muted, CursorLineNumber: base.Foreground(lipgloss.Color(modalAccent)),
		EndOfBuffer: base, Prompt: base.Foreground(lipgloss.Color(modalAccent)),
		Selection: base.Background(lipgloss.Color("#505050")),
	}
	styles := textarea.DefaultDarkStyles()
	styles.Focused, styles.Blurred = state, state
	styles.Cursor.Color = lipgloss.Color(modalAccent)
	editor.SetStyles(styles)
	editor.Prompt = " "
	searchInput := textinput.New()
	searchInput.Prompt = "/"
	searchInput.Placeholder = "Regex pattern"
	searchInput.SetVirtualCursor(true)
	searchInput.SetWidth(99)
	return model{
		ctx: ctx, client: client, comments: make(map[int]string), editor: editor,
		viewport:    newDiffViewport(),
		searchInput: searchInput,
		spinner:     spinner.New(spinner.WithSpinner(spinner.Dot)),
		activity:    loading, loadID: 1, width: 100, height: 30, notice: "Loading PR…",
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.load())
}

func (m model) load() tea.Cmd {
	comments := maps.Clone(m.comments)
	return func() tea.Msg {
		result, err := m.client.load(m.ctx)
		message := loadedMsg{ID: m.loadID, Err: err}
		if err != nil {
			return message
		}

		message.Comments, message.Err = retainComments(m.ctx, m.snapshot, result, comments)
		if message.Err == nil {
			message.Prepared, message.Err = prepareSnapshot(m.ctx, result)
		}

		return message
	}
}

func (m model) request(event reviewEvent) (request reviewRequest) {
	request.CommitID, request.Event = m.snapshot.PR.Head.SHA, event
	if event == commentEvent {
		request.Body = "Inline review comments."
	}

	for _, index := range slices.Sorted(maps.Keys(m.comments)) {
		request.Comments = append(request.Comments, inlineComment{anchor: m.snapshot.Rows[index].anchor, Body: m.comments[index]})
	}

	return
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case spinner.TickMsg:
		if !m.isActive(loading) && !m.isActive(findingSearch) {
			return m, nil
		}

		var command tea.Cmd
		m.spinner, command = m.spinner.Update(msg)
		return m, command
	case tea.WindowSizeMsg:
		m.width, m.height = max(1, msg.Width), max(1, msg.Height)
		m.resizeEditor()
		m.searchInput.SetWidth(max(1, m.width-1))
		m.viewport.SetWidth(max(1, m.width-2))
		m.viewport.SetHeight(max(1, m.height-4))
		m.viewport.SetYOffset(m.viewport.YOffset())
		m.viewport.SetXOffset(m.viewport.XOffset())
	case tea.MouseWheelMsg:
		if m.activity != browsing {
			return m, nil
		}

		m.viewport, _ = m.viewport.Update(msg)
		m.selected = max(m.viewport.YOffset(), min(m.selected, m.viewport.YOffset()+m.viewport.Height()-1))
	case tea.MouseClickMsg:
		if m.activity != browsing || m.width < 20 || m.height < 12 {
			return m, nil
		}

		if msg.Button != tea.MouseLeft && msg.Button != tea.MouseMiddle && msg.Button != tea.MouseRight {
			return m, nil
		}

		if msg.X < 0 || msg.X >= m.width || msg.Y < 2 || msg.Y >= 2+m.viewport.Height() {
			return m, nil
		}

		index := m.viewport.YOffset() + msg.Y - 2
		if index >= len(m.snapshot.Rows) {
			return m, nil
		}

		if msg.Button != tea.MouseLeft && m.snapshot.Rows[index].Kind != codeRow {
			return m, nil
		}

		m.selected = index
		if msg.Button == tea.MouseRight {
			command := m.editComment()
			return m, command
		}

		if msg.Button == tea.MouseMiddle {
			delete(m.comments, index)
			m.notice = "Local comment removed; GitHub comments are read-only."
		}
	case searchResultsMsg:
		if msg.ID != m.searchID || !m.isActive(findingSearch) {
			return m, nil
		}

		if msg.Err != nil {
			m.finishActivity(fmt.Sprintf("Search failed: %v", msg.Err))
			return m, nil
		}

		m.searchPattern, m.matches = msg.Pattern, msg.Matches
		m.jumpMatch(1, true)
		m.finishActivity(m.notice)
	case loadedMsg:
		if msg.ID != m.loadID || !m.isActive(loading) {
			return m, nil
		}

		if msg.Err != nil {
			m.finishActivity(fmt.Sprintf("Load failed: %v. Press r to retry.", msg.Err))
			return m, nil
		}

		m.finishActivity(fmt.Sprintf("Refreshed: kept %d local comment(s), discarded %d.", len(msg.Comments), len(m.comments)-len(msg.Comments)))
		m.comments = msg.Comments
		m.setSnapshot(msg.Prepared)
		m.selected = 0
		m.viewport.SetYOffset(0)
		m.viewport.SetXOffset(0)
		m.blocked = false
	case submittedMsg:
		m.activity = browsing
		if msg.Err == nil {
			m.submittedEvent = msg.Event
			return m, tea.Quit
		}

		m.blocked = errors.Is(msg.Err, errHeadChanged)
		m.submissionErr = msg.Sent
		m.notice = msg.Err.Error()
		if msg.Sent {
			m.notice = fmt.Sprintf("%v. Check GitHub before retrying; the request may have reached it. Press ctrl+r to allow a retry.", msg.Err)
		}
	case tea.KeyPressMsg:
		key := msg.String()
		if m.activity == submitting {
			return m, nil
		}

		if m.activity == enteringSearch {
			switch key {
			case "esc", "ctrl+c":
				m.clearSearch()
				return m, nil
			case "enter":
				pattern := m.searchInput.Value()
				if pattern == "" {
					m.clearSearch()
					return m, nil
				}

				expression, err := regexp.Compile(fmt.Sprintf("(?i)%s", pattern))
				if err != nil {
					m.notice = fmt.Sprintf("Invalid regex: %v", err)
					return m, nil
				}

				m.searchInput.Blur()
				m.searchID++
				m.activity, m.notice = findingSearch, "Searching…"
				return m, tea.Batch(m.spinner.Tick, func() tea.Msg {
					matches, err := findMatches(m.ctx, m.snapshot.Rows, expression)
					return searchResultsMsg{ID: m.searchID, Pattern: pattern, Matches: matches, Err: err}
				})
			}

			var command tea.Cmd
			m.searchInput, command = m.searchInput.Update(msg)
			return m, command
		}

		if m.activity == confirmQuit {
			switch key {
			case "y", "ctrl+c":
				return m, tea.Quit
			case "n", "esc", "q":
				m.activity, m.notice = m.quitFrom, m.quitNotice
			}

			return m, nil
		}

		if m.activity == editing {
			switch key {
			case "esc":
				m.activity, m.notice = browsing, "Edit cancelled."
				m.editor.Blur()
				return m, nil
			case "enter", "ctrl+s":
				body := strings.TrimSpace(m.editor.Value())
				if body == "" {
					m.notice = "Comment is empty; esc cancels, x removes a saved comment."
					return m, nil
				}

				m.comments[m.selected] = body
				m.activity, m.notice = browsing, "Comment saved locally."
				m.editor.Blur()
				return m, nil
			case "ctrl+c":
				m.confirmQuit()
				return m, nil
			}
		} else {
			switch key {
			case "esc":
				if m.activity == browsing || m.activity == findingSearch {
					m.clearSearch()
				}

				return m, nil
			case "q", "ctrl+c":
				if len(m.comments) == 0 {
					return m, tea.Quit
				}

				m.confirmQuit()
				return m, nil
			}

			if m.activity == loading || m.activity == findingSearch {
				return m, nil
			}

			switch key {
			case "/":
				m.activity, m.notice = enteringSearch, ""
				m.searchInput.SetValue(m.searchPattern)
				command := m.searchInput.Focus()
				return m, command
			case "n":
				m.jumpMatch(1, false)
			case "p":
				m.jumpMatch(-1, false)
			case "j", "down":
				m.selected++
			case "k", "up":
				m.selected--
			case "pgdown", "ctrl+d":
				m.selected += max(1, m.viewport.Height()/2)
			case "pgup", "ctrl+u":
				m.selected -= max(1, m.viewport.Height()/2)
			case "g", "home":
				m.selected = 0
			case "G", "end":
				m.selected = len(m.snapshot.Rows) - 1
			case "h", "left":
				m.viewport.ScrollLeft(8)
			case "l", "right":
				m.viewport.ScrollRight(8)
			case "[", "]":
				direction := 1
				if key == "[" {
					direction = -1
				}

				m.moveFile(direction)
			case "c":
				command := m.editComment()
				return m, command
			case "x":
				delete(m.comments, m.selected)
				m.notice = "Local comment removed; GitHub comments are read-only."
			case "r":
				m.loadID++
				m.activity, m.notice = loading, "Refreshing PR and matching local comments…"
				return m, tea.Batch(m.spinner.Tick, m.load())
			case "ctrl+r":
				if m.submissionErr {
					m.submissionErr, m.notice = false, "Retry unlocked. a approves; s submits comments."
				}
			case "a", "s":
				if m.blocked {
					m.notice = errHeadChanged.Error()
					return m, nil
				}

				if m.submissionErr {
					m.notice = "Check GitHub before retrying; press ctrl+r to unlock another submission."
					return m, nil
				}

				if m.snapshot.PR.Head.SHA == "" {
					m.notice = "Press r to load the PR before submitting."
					return m, nil
				}

				event := approvalEvent
				if key == "s" {
					event = commentEvent
					if len(m.comments) == 0 {
						m.notice = "Add at least one inline comment before submitting comments only."
						return m, nil
					}
				}

				m.activity, m.notice = submitting, "Checking PR head and submitting review…"
				request := m.request(event)
				return m, func() tea.Msg {
					sent, err := m.client.submitReview(m.ctx, m.snapshot.PR, request)
					return submittedMsg{Sent: sent, Err: err, Event: event}
				}
			}
		}
	}

	if m.activity == editing {
		var command tea.Cmd
		m.editor, command = m.editor.Update(msg)
		return m, command
	}

	if m.activity == enteringSearch {
		var command tea.Cmd
		m.searchInput, command = m.searchInput.Update(msg)
		return m, command
	}

	m.selected = max(0, min(m.selected, len(m.snapshot.Rows)-1))
	if m.selected < m.viewport.YOffset() {
		m.viewport.SetYOffset(m.selected)
	} else if m.selected >= m.viewport.YOffset()+m.viewport.Height() {
		m.viewport.SetYOffset(m.selected - m.viewport.Height() + 1)
	}

	return m, nil
}

func (m *model) clearSearch() {
	m.searchID++
	m.searchPattern, m.matches = "", nil
	m.searchInput.SetValue("")
	m.searchInput.Blur()
	m.activity, m.notice = browsing, "Search cleared."
}

func (m model) isActive(state activity) bool {
	return m.activity == state || m.activity == confirmQuit && m.quitFrom == state
}

func (m *model) confirmQuit() {
	m.quitFrom, m.quitNotice = m.activity, m.notice
	m.activity, m.notice = confirmQuit, "Discard local comments and quit? y / n"
}

func (m *model) finishActivity(notice string) {
	if m.activity == confirmQuit {
		m.quitFrom, m.quitNotice = browsing, notice
		m.notice = "Discard local comments and quit? y / n"
		return
	}

	m.activity, m.notice = browsing, notice
}

type textOffset struct {
	Byte   int
	Column int
}

type displayLine struct {
	Text    string
	Offsets []textOffset
}

// Text has already been sanitised; checkpoints always fall between graphemes.
func indexDisplayLine(ctx context.Context, text string) (line displayLine, err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	line.Text = text
	if len(text) <= 256 {
		return
	}

	position := textOffset{}
	line.Offsets = append(line.Offsets, position)
	for position.Byte < len(text) {
		if err = ctx.Err(); err != nil {
			return
		}

		cluster, width := ansi.FirstGraphemeCluster(text[position.Byte:], ansi.GraphemeWidth)
		position.Byte += len(cluster)
		position.Column += width
		if position.Column-line.Offsets[len(line.Offsets)-1].Column >= 256 {
			line.Offsets = append(line.Offsets, position)
		}
	}

	if line.Offsets[len(line.Offsets)-1].Byte != position.Byte {
		line.Offsets = append(line.Offsets, position)
	}

	return
}

func (line displayLine) cut(left, right int) string {
	if len(line.Offsets) == 0 {
		return ansi.Cut(line.Text, left, right)
	}

	compareColumn := func(offset textOffset, column int) int { return cmp.Compare(offset.Column, column) }
	start, found := slices.BinarySearchFunc(line.Offsets, left, compareColumn)
	if !found {
		start--
	}

	end, _ := slices.BinarySearchFunc(line.Offsets, right, compareColumn)
	end = min(end, len(line.Offsets)-1)
	begin, stop := line.Offsets[start], line.Offsets[end]
	return ansi.Cut(line.Text[begin.Byte:stop.Byte], left-begin.Column, right-begin.Column)
}

type preparedSnapshot struct {
	Snapshot snapshot
	Lines    []displayLine
	Files    []int
	Viewport viewport.Model
}

func newDiffViewport() viewport.Model {
	diffViewport := viewport.New(viewport.WithWidth(98), viewport.WithHeight(26))
	diffViewport.KeyMap = viewport.KeyMap{}
	diffViewport.FillHeight = true
	diffViewport.SetHorizontalStep(8)
	return diffViewport
}

func prepareSnapshot(ctx context.Context, next snapshot) (prepared preparedSnapshot, err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	prepared.Snapshot = next
	prepared.Viewport = newDiffViewport()
	var lines []string
	for i, row := range next.Rows {
		if err = ctx.Err(); err != nil {
			return
		}

		line, subErr := indexDisplayLine(ctx, renderRow(row))
		if subErr != nil {
			err = subErr
			return
		}

		prepared.Lines = append(prepared.Lines, line)
		lines = append(lines, line.Text)
		if row.Kind == fileRow {
			prepared.Files = append(prepared.Files, i)
		}
	}

	if len(lines) == 0 {
		text := "No diff lines to display. Approval is available once loaded."
		lines = append(lines, text)
		prepared.Lines = append(prepared.Lines, displayLine{Text: text})
	}

	prepared.Viewport.SetContentLines(lines)
	err = ctx.Err()
	return
}

func (m *model) setSnapshot(next preparedSnapshot) {
	m.snapshot = next.Snapshot
	m.lines, m.files, m.viewport = next.Lines, next.Files, next.Viewport
	m.searchID++
	m.searchPattern, m.matches = "", nil
	m.viewport.SetWidth(max(1, m.width-2))
	m.viewport.SetHeight(max(1, m.height-4))
	m.viewport.SetYOffset(m.viewport.YOffset())
	m.viewport.SetXOffset(m.viewport.XOffset())
}

func (m model) diffView() string {
	var lines []string
	width := m.viewport.Width()
	left := m.viewport.XOffset()
	for offset := range m.viewport.Height() {
		index := m.viewport.YOffset() + offset
		line := ""
		if index < len(m.lines) {
			text := m.lines[index].cut(left, left+width)
			line = fmt.Sprint(m.gutter(viewport.GutterContext{Index: index}), m.rowStyle(index).Render(text))
		}

		lines = append(lines, line)
	}

	return lipgloss.NewStyle().Width(m.width).Render(strings.Join(lines, "\n"))
}

func findMatches(ctx context.Context, rows []diffRow, expression *regexp.Regexp) (matches []searchMatch, err error) {
	if err = ctx.Err(); err != nil {
		return
	}

	for i, row := range rows {
		if err = ctx.Err(); err != nil {
			return
		}

		text := row.Text
		if row.Kind == codeRow {
			text = text[1:]
		}

		match := expression.FindStringIndex(text)
		if match == nil {
			continue
		}

		displayed := strings.ReplaceAll(safeText(text), "\n", " ↵ ")
		prefixWidth := lipgloss.Width(renderRow(row)) - lipgloss.Width(displayed)
		column := prefixWidth + lipgloss.Width(strings.ReplaceAll(safeText(text[:match[0]]), "\n", " ↵ "))
		matches = append(matches, searchMatch{Row: i, Column: column})
	}

	return
}

func (m *model) jumpMatch(direction int, includeCurrent bool) {
	if len(m.matches) == 0 {
		m.notice = "No matching lines. Press / to search."
		return
	}

	index, found := slices.BinarySearchFunc(m.matches, m.selected, func(match searchMatch, row int) int { return cmp.Compare(match.Row, row) })
	if direction < 0 {
		index--
	} else if found && !includeCurrent {
		index++
	}

	index = (index + len(m.matches)) % len(m.matches)
	match := m.matches[index]
	m.selected = match.Row
	m.viewport.SetXOffset(max(0, match.Column-m.viewport.Width()/2))
	m.notice = fmt.Sprintf("Matching line %d/%d · /%s · n next · p previous", index+1, len(m.matches), m.searchPattern)
}

func (m model) fileIndex() int {
	index, found := slices.BinarySearch(m.files, m.selected)
	if !found {
		index--
	}

	return index
}

func (m *model) moveFile(direction int) {
	index := m.fileIndex() + direction
	if index >= 0 && index < len(m.files) {
		m.selected = m.files[index]
		m.viewport.SetXOffset(0)
	}
}

func safeText(text string) string {
	text = strings.ReplaceAll(text, "\t", "    ")
	return strings.Map(func(r rune) rune {
		if r != '\n' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) {
			return '�'
		}

		return r
	}, text)
}

func renderRow(row diffRow) string {
	text := strings.ReplaceAll(safeText(row.Text), "\n", " ↵ ")
	switch row.Kind {
	case fileRow:
		text = fmt.Sprintf("── %s ──", text)
	case codeRow:
		oldLine, newLine := "", ""
		if row.OldLine > 0 {
			oldLine = fmt.Sprint(row.OldLine)
		}

		if row.NewLine > 0 {
			newLine = fmt.Sprint(row.NewLine)
		}

		text = fmt.Sprintf("%5s %5s │ %s", oldLine, newLine, text)
	case commentRow:
		text = fmt.Sprintf("  GitHub │ %s", text)
	}

	return text
}

func (m model) rowStyle(index int) lipgloss.Style {
	style := lipgloss.NewStyle()
	if index >= len(m.snapshot.Rows) {
		return style
	}

	row := m.snapshot.Rows[index]
	switch row.Kind {
	case fileRow:
		style = style.Foreground(lipgloss.Color("#89b4fa")).Bold(true)
	case hunkRow:
		style = style.Foreground(lipgloss.Color("#94e2d5"))
	case codeRow:
		if strings.HasPrefix(row.Text, "+") {
			style = style.Foreground(lipgloss.Color("#a6e3a1"))
		} else if strings.HasPrefix(row.Text, "-") {
			style = style.Foreground(lipgloss.Color("#f38ba8"))
		}
	case commentRow:
		style = style.Foreground(lipgloss.Color("#f9e2af"))
	}

	if _, found := slices.BinarySearchFunc(m.matches, index, func(match searchMatch, row int) int { return cmp.Compare(match.Row, row) }); found {
		style = style.Underline(true)
	}

	if index == m.selected {
		style = style.Background(lipgloss.Color("#45475a")).Bold(true)
	}

	return style
}

func (m model) gutter(info viewport.GutterContext) string {
	index := info.Index
	if index >= len(m.snapshot.Rows) {
		return "  "
	}

	marker := " "
	if _, ok := m.comments[index]; ok {
		marker = "●"
	} else if index+1 < len(m.snapshot.Rows) && m.snapshot.Rows[index+1].Kind == commentRow && m.snapshot.Rows[index].Kind == codeRow {
		marker = "○"
	}

	return fmt.Sprintf("%s ", marker)
}

func (m *model) editComment() tea.Cmd {
	if len(m.snapshot.Rows) == 0 || m.snapshot.Rows[m.selected].Kind != codeRow {
		m.notice = "Select a diff code line to add a comment."
		return nil
	}

	m.activity, m.notice = editing, ""
	m.resizeEditor()
	m.editor.SetValue(m.comments[m.selected])
	return m.editor.Focus()
}

func (m *model) resizeEditor() {
	m.editor.SetWidth(max(1, min(76, m.width-8)))
	m.editor.SetHeight(max(1, min(10, m.height-14)))
}

func (m model) commentModal() string {
	width := max(1, min(76, m.width-8))
	row := m.snapshot.Rows[m.selected]
	title := fmt.Sprintf("Comment · %s:%d (%s)", safeText(row.Path), row.Line, row.Side)
	contents := []string{
		lipgloss.NewStyle().Foreground(lipgloss.Color(modalAccent)).Bold(true).Render(ansi.Truncate(title, width, "…")),
		lipgloss.NewStyle().Foreground(lipgloss.Color(modalMuted)).Render(ansi.Truncate(safeText(row.Text), width, "…")),
		"",
		m.editor.View(),
		ansi.Truncate(m.notice, width, "…"),
		ansi.Truncate("enter save · shift+enter new line · esc cancel", width, "…"),
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(modalAccent)).
		Background(lipgloss.Color(modalBackground)).
		Foreground(lipgloss.Color(modalForeground)).
		Padding(1)
	modal := box.Render(strings.Join(contents, "\n"))
	modalWidth, modalHeight := lipgloss.Size(modal)
	canvas := lipgloss.NewCanvas(modalWidth, modalHeight).Compose(lipgloss.NewLayer(modal))
	// Textarea placeholder and padding cells can have no background of their own.
	for y := 1; y < modalHeight-1; y++ {
		for x := 1; x < modalWidth-1; x++ {
			cell := uv.Cell{Content: " ", Width: 1}
			if existing := canvas.CellAt(x, y); existing != nil {
				cell = *existing
			}

			if cell.Style.Bg == nil {
				cell.Style.Bg = lipgloss.Color(modalBackground)
				canvas.SetCell(x, y, &cell)
			}

			x += max(0, cell.Width-1)
		}
	}

	return canvas.Render()
}

func (m model) View() tea.View {
	if m.activity == loading {
		return m.screen(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			fmt.Sprintf("%s %s\n%s #%d · q quit", m.spinner.View(), m.notice, m.client.Repo, m.client.Number)))
	}

	if m.snapshot.PR.Head.SHA == "" {
		return m.screen(lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center,
			fmt.Sprintf("%s\nr retry · q quit", safeText(m.notice))))
	}

	header := fmt.Sprintf("%s #%d · %s · %d local comment(s)", m.client.Repo, m.client.Number, safeText(m.snapshot.PR.Title), len(m.comments))
	lines := []string{lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#cba6f7")).Render(ansi.Truncate(header, m.width, "…"))}
	file := ""
	if index := m.fileIndex(); index >= 0 {
		file = m.snapshot.Rows[m.files[index]].Text
	}

	lines = append(lines, ansi.Truncate(fmt.Sprintf("%s · line %d/%d", safeText(file), min(m.selected+1, len(m.snapshot.Rows)), len(m.snapshot.Rows)), m.width, "…"))
	lines = append(lines, m.diffView())

	status := m.notice
	if status == "" {
		status = "● local comment · ○ GitHub comment · mouse wheel scrolls · h/l horizontal"
		if comment, ok := m.comments[m.selected]; ok {
			status = fmt.Sprintf("Local: %s", comment)
		}
	}

	help := "j k move · [ ] prev/next file · c comment · x remove · r refresh · / search · n p match · a approve · s submit · q quit"
	if m.submissionErr {
		help = "Check GitHub: submission may have succeeded. ctrl+r unlocks retry · q quit"
	}

	if m.activity == editing {
		status, help = "", ""
	}

	statusLine := ansi.Truncate(strings.ReplaceAll(safeText(status), "\n", " "), m.width, "…")
	if m.activity == enteringSearch {
		statusLine = m.searchInput.View()
		help = "enter search · esc cancel · empty pattern clears search"
		if m.notice != "" {
			help = m.notice
		}
	} else if m.activity == findingSearch {
		statusLine = fmt.Sprintf("%s Searching…", m.spinner.View())
	}

	lines = append(lines, ansi.Truncate(statusLine, m.width, "…"), ansi.Truncate(help, m.width, "…"))
	content := strings.Join(lines, "\n")
	if m.activity == editing {
		modal := m.commentModal()
		x := max(0, (m.width-lipgloss.Width(modal))/2)
		y := max(0, (m.height-lipgloss.Height(modal))/2)
		content = lipgloss.NewCompositor(
			lipgloss.NewLayer(content),
			lipgloss.NewLayer(modal).X(x).Y(y).Z(1),
		).Render()
	}

	if m.width < 20 || m.height < 12 {
		content = ansi.Truncate("Enlarge the terminal to review.", m.width, "…")
	}

	return m.screen(content)
}

func (m model) screen(content string) tea.View {
	view := tea.NewView(lipgloss.NewStyle().Width(m.width).Height(m.height).MaxWidth(m.width).MaxHeight(m.height).Render(content))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}
