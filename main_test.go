package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleDiff = `diff --git a/example.go b/example.go
index 1234567..abcdef0 100644
--- a/example.go
+++ b/example.go
@@ -5,3 +5,3 @@ func example() {
 context
-removed
+added
 tail
@@ -20 +20,2 @@
 other
+extra
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-gone
`

func parsedSnapshot(t *testing.T, text, sha string) snapshot {
	t.Helper()
	rows, err := parseDiff([]byte(text))
	require.NoError(t, err)
	pr := pullRequest{Title: "Example PR", State: "open"}
	pr.Head.SHA, pr.Base.SHA = sha, "base"
	return snapshot{PR: pr, Rows: rows}
}

func codeIndex(t *testing.T, rows []diffRow, text string) int {
	t.Helper()
	index := slices.IndexFunc(rows, func(row diffRow) bool { return row.Kind == codeRow && row.Text == text })
	require.NotEqual(t, -1, index, "missing code line %q", text)
	return index
}

func setSnapshot(t testing.TB, m *model, next snapshot) {
	t.Helper()
	prepared, err := prepareSnapshot(t.Context(), next)
	require.NoError(t, err)
	m.setSnapshot(prepared)
}

func retainedComments(t *testing.T, previous, next snapshot, comments map[int]string) map[int]string {
	t.Helper()
	retained, err := retainComments(t.Context(), previous, next, comments)
	require.NoError(t, err)
	return retained
}

func loadResult(t testing.TB, m model, next snapshot) loadedMsg {
	t.Helper()
	prepared, err := prepareSnapshot(t.Context(), next)
	require.NoError(t, err)
	comments, err := retainComments(t.Context(), m.snapshot, next, m.comments)
	require.NoError(t, err)
	return loadedMsg{ID: m.loadID, Prepared: prepared, Comments: comments}
}

func TestParseDiffAnchors(t *testing.T) {
	t.Parallel()
	snapshot := parsedSnapshot(t, sampleDiff, "head")
	var anchors []anchor
	for _, row := range snapshot.Rows {
		if row.Kind == codeRow {
			anchors = append(anchors, row.anchor)
		}
	}

	expected := []anchor{
		{Path: "example.go", Side: "RIGHT", Line: 5},
		{Path: "example.go", Side: "LEFT", Line: 6},
		{Path: "example.go", Side: "RIGHT", Line: 6},
		{Path: "example.go", Side: "RIGHT", Line: 7},
		{Path: "example.go", Side: "RIGHT", Line: 20},
		{Path: "example.go", Side: "RIGHT", Line: 21},
		{Path: "gone.txt", Side: "LEFT", Line: 1},
	}
	if difference := cmp.Diff(expected, anchors); difference != "" {
		t.Errorf("anchors mismatch (-expected +actual):\n%s", difference)
	}
}

func TestDiffEdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		diff      string
		path      string
		codeLines int
		invalid   bool
	}{
		{name: "empty"},
		{name: "new file", diff: "diff --git a/new b/new\nnew file mode 100644\n--- /dev/null\n+++ b/new\n@@ -0,0 +1 @@\n+new\n", path: "new", codeLines: 1},
		{name: "no newline", diff: "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n", path: "a", codeLines: 2},
		{name: "rename", diff: "diff --git a/old b/new\nsimilarity index 100%\nrename from old\nrename to new\n", path: "new"},
		{name: "binary", diff: "diff --git a/image.png b/image.png\nindex 1234567..abcdef0 100644\nBinary files a/image.png and b/image.png differ\n", path: "image.png"},
		{name: "quoted path", diff: "diff --git \"a/a\\tb\" \"b/a\\tb\"\n--- \"a/a\\tb\"\n+++ \"b/a\\tb\"\n@@ -1 +1 @@\n-old\n+new\n", path: "a\tb", codeLines: 2},
		{name: "truncated hunk", diff: "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1,3 +1,3 @@\n only one\n", invalid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rows, err := parseDiff([]byte(test.diff))
			if test.invalid {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			count := 0
			for _, row := range rows {
				if row.Kind == fileRow {
					assert.Equal(t, test.path, row.Path)
				}

				if row.Kind == codeRow {
					count++
				}
			}

			assert.Equal(t, test.codeLines, count)
		})
	}
}

func TestRefreshRetainsSurvivingAnchors(t *testing.T) {
	t.Parallel()
	previous := parsedSnapshot(t, sampleDiff, "old")
	nextDiff := strings.ReplaceAll(sampleDiff, "@@ -5,3 +5,3 @@", "@@ -8,3 +8,3 @@")
	nextDiff = strings.ReplaceAll(nextDiff, "+extra", "+changed")
	next := parsedSnapshot(t, nextDiff, "new")
	comments := map[int]string{
		codeIndex(t, previous.Rows, "+added"):   "addition",
		codeIndex(t, previous.Rows, " context"): "context",
		codeIndex(t, previous.Rows, "-removed"): "deletion",
		codeIndex(t, previous.Rows, "+extra"):   "discard",
	}
	retained := retainedComments(t, previous, next, comments)
	assert.Len(t, retained, 3)
	assert.Equal(t, "addition", retained[codeIndex(t, next.Rows, "+added")])
	assert.Equal(t, "context", retained[codeIndex(t, next.Rows, " context")])
	assert.Equal(t, "deletion", retained[codeIndex(t, next.Rows, "-removed")])
	assert.Equal(t, 9, next.Rows[codeIndex(t, next.Rows, "+added")].Line)
}

func TestRefreshAmbiguousAnchors(t *testing.T) {
	t.Parallel()
	text := "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -0,0 +1,7 @@\n+x\n+x\n+x\n+x\n+x\n+x\n+x\n"
	previous := parsedSnapshot(t, text, "old")
	index := codeIndex(t, previous.Rows, "+x") + 3
	next := parsedSnapshot(t, text, "new")
	assert.Empty(t, retainedComments(t, previous, next, map[int]string{index: "ambiguous"}))
	next.PR.Head.SHA = previous.PR.Head.SHA
	assert.Equal(t, map[int]string{index: "unchanged"}, retainedComments(t, previous, next, map[int]string{index: "unchanged"}))
}

func TestRefreshUsesNeighbourhood(t *testing.T) {
	t.Parallel()
	text := "diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -0,0 +1,6 @@\n+first\n+repeat\n+after first\n+second\n+repeat\n+after second\n"
	previous := parsedSnapshot(t, text, "old")
	next := parsedSnapshot(t, strings.Replace(text, "+1,6", "+10,6", 1), "new")
	index := codeIndex(t, previous.Rows, "+repeat")
	assert.Equal(t, map[int]string{index: "keep"}, retainedComments(t, previous, next, map[int]string{index: "keep"}))
}

func TestExistingComments(t *testing.T) {
	t.Parallel()
	snapshot := parsedSnapshot(t, sampleDiff, "head")
	comments := []githubComment{
		{Path: "example.go", Side: "RIGHT", Line: 6, Body: "current\nsecond line"},
		{Path: "gone.txt", Side: "LEFT", Line: 1, Body: "deleted"},
		{Path: "example.go", Side: "RIGHT", OriginalLine: 99, Body: "outdated"},
		{Body: "general discussion"},
		{Body: "review summary", State: "COMMENTED"},
		{Body: "hidden pending", State: "PENDING"},
	}
	comments[0].User.Login = "reviewer"
	rows := attachComments(snapshot.Rows, comments)
	index := codeIndex(t, rows, "+added")
	assert.Equal(t, "@reviewer: current", rows[index+1].Text)
	assert.Equal(t, "second line", rows[index+2].Text)
	var texts []string
	for _, row := range rows {
		texts = append(texts, row.Text)
	}

	joined := strings.Join(texts, "\n")
	assert.Contains(t, joined, "outdated or file comment")
	assert.Contains(t, joined, "general discussion")
	assert.Contains(t, joined, "review summary")
	assert.NotContains(t, joined, "hidden pending")
	next := snapshot
	next.Rows = rows
	oldIndex := codeIndex(t, snapshot.Rows, "-gone")
	assert.Equal(t, map[int]string{codeIndex(t, rows, "-gone"): "local"}, retainedComments(t, snapshot, next, map[int]string{oldIndex: "local"}))
}

func press(t *testing.T, m model, code rune, mod tea.KeyMod) (model, tea.Cmd) {
	t.Helper()
	updated, command := m.Update(tea.KeyPressMsg{Code: code, Mod: mod})
	result, ok := updated.(model)
	require.True(t, ok)
	return result, command
}

func TestCommentEditorAndNavigation(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	m.selected = codeIndex(t, m.snapshot.Rows, "+added")
	m, _ = press(t, m, 'c', 0)
	assert.Equal(t, editing, m.activity)
	m.editor.SetValue("first\nsecond")
	m, _ = press(t, m, 's', tea.ModCtrl)
	assert.Equal(t, browsing, m.activity)
	assert.Equal(t, "first\nsecond", m.comments[m.selected])
	assert.Contains(t, m.View().Content, "●")
	m, _ = press(t, m, 'c', 0)
	assert.Equal(t, "first\nsecond", m.editor.Value())
	m.editor.SetValue("cancelled")
	m, _ = press(t, m, tea.KeyEscape, 0)
	assert.Equal(t, "first\nsecond", m.comments[m.selected])
	m, _ = press(t, m, 'x', 0)
	assert.Empty(t, m.comments)
	m, _ = press(t, m, ']', 0)
	assert.Equal(t, "gone.txt", m.snapshot.Rows[m.selected].Path)
	m, _ = press(t, m, '[', 0)
	assert.Equal(t, "example.go", m.snapshot.Rows[m.selected].Path)
	m.snapshot.Rows = nil
	setSnapshot(t, &m, m.snapshot)
	m, _ = press(t, m, ']', 0)
	assert.Equal(t, 0, m.selected)
}

func TestMouseScrolling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		activity   activity
		wheel      tea.MouseWheelMsg
		selected   int
		horizontal int
	}{
		{name: "down", wheel: tea.MouseWheelMsg{Button: tea.MouseWheelDown}, selected: 3},
		{name: "up clamps", wheel: tea.MouseWheelMsg{Button: tea.MouseWheelUp}, selected: 1},
		{name: "right", wheel: tea.MouseWheelMsg{Button: tea.MouseWheelRight}, selected: 1, horizontal: 8},
		{name: "left clamps", wheel: tea.MouseWheelMsg{Button: tea.MouseWheelLeft}, selected: 1},
		{name: "editing keeps anchor", activity: editing, wheel: tea.MouseWheelMsg{Button: tea.MouseWheelDown}, selected: 1},
		{name: "loading", activity: loading, wheel: tea.MouseWheelMsg{Button: tea.MouseWheelDown}, selected: 1},
		{name: "submitting", activity: submitting, wheel: tea.MouseWheelMsg{Button: tea.MouseWheelDown}, selected: 1},
		{name: "quit confirmation", activity: confirmQuit, wheel: tea.MouseWheelMsg{Button: tea.MouseWheelDown}, selected: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			m := newModel(context.Background(), githubPR{})
			setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
			resized, _ := m.Update(tea.WindowSizeMsg{Width: 20, Height: 8})
			m = resized.(model)
			m.activity, m.selected = test.activity, 1
			updated, _ := m.Update(test.wheel)
			actual := updated.(model)
			assert.Equal(t, test.selected, actual.selected)
			assert.Equal(t, test.horizontal, actual.viewport.XOffset())
		})
	}

	m := newModel(context.Background(), githubPR{})
	m.activity = browsing
	updated, _ := m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Zero(t, updated.(model).selected, "empty diff")
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.selected = len(m.snapshot.Rows) - 1
	m.viewport.GotoBottom()
	updated, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	assert.Equal(t, m.selected, updated.(model).selected, "end of diff")
}

func TestClickCommentsOnScrolledDiff(t *testing.T) {
	t.Parallel()
	for _, text := range []string{" context", "-removed", "+added"} {
		t.Run(text, func(t *testing.T) {
			t.Parallel()
			m := newModel(context.Background(), githubPR{})
			snapshot := parsedSnapshot(t, sampleDiff, "head")
			snapshot.Rows = attachComments(snapshot.Rows, []githubComment{{Path: "example.go", Side: "RIGHT", Line: 5, Body: strings.Repeat("Existing comment ", 10)}})
			setSnapshot(t, &m, snapshot)
			m.activity = browsing
			updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 14})
			m = updated.(model)
			m.viewport.SetYOffset(2)
			m.viewport.SetXOffset(16)
			index := codeIndex(t, m.snapshot.Rows, text)
			m.comments[index] = "saved comment"
			click := tea.MouseClickMsg{X: 30, Y: 2 + index - m.viewport.YOffset(), Button: tea.MouseLeft}
			updated, _ = m.Update(click)
			m = updated.(model)
			assert.Equal(t, browsing, m.activity)
			assert.Equal(t, index, m.selected)
			assert.False(t, m.editor.Focused())
			click.Button = tea.MouseRight
			updated, _ = m.Update(click)
			m = updated.(model)
			assert.Equal(t, editing, m.activity)
			assert.Equal(t, index, m.selected)
			assert.Equal(t, "saved comment", m.editor.Value())
			assert.True(t, m.editor.Focused())
			m.editor.SetValue("edited comment")
			m, _ = press(t, m, 's', tea.ModCtrl)
			request := m.request(commentEvent)
			require.Len(t, request.Comments, 1)
			assert.Equal(t, m.snapshot.Rows[index].anchor, request.Comments[0].anchor)
			m.comments[codeIndex(t, m.snapshot.Rows, "-gone")] = "keep this"
			click.Button = tea.MouseMiddle
			updated, _ = m.Update(click)
			m = updated.(model)
			assert.NotContains(t, m.comments, index)
			assert.Len(t, m.comments, 1)
		})
	}
}

func TestClicksIgnoreNonCodeAndBusyStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		mode  activity
		click tea.MouseClickMsg
	}{
		{name: "title", click: tea.MouseClickMsg{X: 1, Y: 0, Button: tea.MouseLeft}},
		{name: "file header", click: tea.MouseClickMsg{X: 1, Y: 2, Button: tea.MouseRight}},
		{name: "hunk", click: tea.MouseClickMsg{X: 1, Y: 3, Button: tea.MouseMiddle}},
		{name: "blank space", click: tea.MouseClickMsg{X: 1, Y: 26, Button: tea.MouseLeft}},
		{name: "footer", click: tea.MouseClickMsg{X: 1, Y: 28, Button: tea.MouseMiddle}},
		{name: "outside", click: tea.MouseClickMsg{X: 100, Y: 4, Button: tea.MouseLeft}},
		{name: "right click on hunk", click: tea.MouseClickMsg{X: 1, Y: 3, Button: tea.MouseRight}},
		{name: "editing", mode: editing, click: tea.MouseClickMsg{X: 1, Y: 4, Button: tea.MouseMiddle}},
		{name: "loading", mode: loading, click: tea.MouseClickMsg{X: 1, Y: 4, Button: tea.MouseLeft}},
		{name: "submitting", mode: submitting, click: tea.MouseClickMsg{X: 1, Y: 4, Button: tea.MouseMiddle}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			m := newModel(context.Background(), githubPR{})
			setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
			m.activity = test.mode
			index := codeIndex(t, m.snapshot.Rows, " context")
			m.comments[index] = "keep"
			updated, _ := m.Update(test.click)
			actual := updated.(model)
			assert.Equal(t, m.activity, actual.activity)
			assert.Equal(t, m.selected, actual.selected)
			assert.Equal(t, "keep", actual.comments[index])
		})
	}
}

func TestCommentModalResizes(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	m.selected = codeIndex(t, m.snapshot.Rows, "+added")
	m, _ = press(t, m, 'c', 0)
	m.editor.SetValue("A comment in the modal")
	for _, size := range []tea.WindowSizeMsg{{Width: 100, Height: 30}, {Width: 60, Height: 24}, {Width: 40, Height: 18}} {
		updated, _ := m.Update(size)
		m = updated.(model)
		modal := m.commentModal()
		assert.Contains(t, modal, "╭")
		assert.Contains(t, modal, "╯")
		assert.LessOrEqual(t, lipgloss.Width(modal), size.Width)
		assert.LessOrEqual(t, lipgloss.Height(modal), size.Height)
		assert.Equal(t, "A comment in the modal", m.editor.Value())
		assert.Equal(t, tea.MouseModeCellMotion, m.View().MouseMode)
	}

	m, _ = press(t, m, 's', tea.ModCtrl)
	assert.Equal(t, "A comment in the modal", m.comments[m.selected])
	assert.Equal(t, browsing, m.activity)
}

func TestLayoutFillsTerminal(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	m.selected = codeIndex(t, m.snapshot.Rows, "+added")
	for _, size := range []tea.WindowSizeMsg{{Width: 100, Height: 40}, {Width: 40, Height: 18}, {Width: 120, Height: 35}, {Width: 12, Height: 6}} {
		updated, _ := m.Update(size)
		m = updated.(model)
		for _, mode := range []activity{browsing, editing} {
			m.activity = mode
			view := m.View()
			assert.Equal(t, size.Width, lipgloss.Width(view.Content))
			assert.Equal(t, size.Height, lipgloss.Height(view.Content))
			assert.True(t, view.AltScreen)
		}
	}
}

func TestPayloadAndSubmissionState(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	for _, text := range []string{" context", "-removed", "+added"} {
		m.comments[codeIndex(t, m.snapshot.Rows, text)] = text
	}

	data, err := json.Marshal(m.request(approvalEvent))
	require.NoError(t, err)
	assert.JSONEq(t, `{"commit_id":"head","event":"APPROVE","comments":[{"path":"example.go","side":"RIGHT","line":5,"body":" context"},{"path":"example.go","side":"LEFT","line":6,"body":"-removed"},{"path":"example.go","side":"RIGHT","line":6,"body":"+added"}]}`, string(data))
	updated, _ := m.Update(submittedMsg{Err: errHeadChanged})
	m = updated.(model)
	m, command := press(t, m, 'a', 0)
	assert.Nil(t, command)
	assert.True(t, m.blocked)
	m, _ = press(t, m, 'r', 0)
	updated, _ = m.Update(loadResult(t, m, m.snapshot))
	m = updated.(model)
	assert.False(t, m.blocked)
	m, command = press(t, m, 'a', 0)
	require.NotNil(t, command)
	assert.Equal(t, submitting, m.activity)
	m, command = press(t, m, 'a', 0)
	assert.Nil(t, command, "do not submit twice while in flight")
	updated, _ = m.Update(submittedMsg{Sent: true, Err: errors.New("connection lost")})
	m = updated.(model)
	m, command = press(t, m, 'a', 0)
	assert.Nil(t, command, "ambiguous failures need acknowledgement before retry")
	assert.True(t, m.submissionErr, "approval must not acknowledge a previous failure")
	m, command = press(t, m, 'r', tea.ModCtrl)
	assert.Nil(t, command)
	assert.False(t, m.submissionErr)
	m.comments = nil
	data, err = json.Marshal(m.request(approvalEvent))
	require.NoError(t, err)
	assert.JSONEq(t, `{"commit_id":"head","event":"APPROVE"}`, string(data))
}

type testTransport func(*http.Request) (*http.Response, error)

func (transport testTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func testGitHub(t *testing.T, handler http.HandlerFunc) githubPR {
	t.Helper()
	client, err := newGitHubPR("owner/repo", 123, api.ClientOptions{
		Host: "github.com", APIHost: "api.github.com", AuthToken: "test-token",
		Transport: testTransport(func(request *http.Request) (*http.Response, error) {
			assert.Contains(t, request.Header.Get("Authorization"), "test-token")
			response := httptest.NewRecorder()
			handler(response, request)
			result := response.Result()
			result.Request = request
			return result, nil
		}),
	})
	require.NoError(t, err)
	return client
}

func TestSDKLoadAndApprove(t *testing.T) {
	t.Parallel()
	var submitted []reviewRequest
	var calls []string
	client := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, fmt.Sprintf("%s %s", r.Method, r.URL.RequestURI()))
		if r.Method == http.MethodPost {
			assert.Equal(t, "/repos/owner/repo/pulls/123/reviews", r.URL.Path)
			var request reviewRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			submitted = append(submitted, request)
			fmt.Fprint(w, `{"state":"APPROVED"}`)
			return
		}

		if r.Header.Get("Accept") == "application/vnd.github.v3.diff" {
			fmt.Fprint(w, sampleDiff)
			return
		}

		if r.URL.Query().Get("per_page") == "100" {
			count := 100
			if r.URL.Query().Get("page") == "2" {
				count = 1
			}

			var comments []githubComment
			for range count {
				comments = append(comments, githubComment{Body: "Existing comment"})
			}

			require.NoError(t, json.NewEncoder(w).Encode(comments))
			return
		}

		fmt.Fprint(w, `{"title":"PR","state":"open","head":{"sha":"head"},"base":{"sha":"base"}}`)
	})
	loaded, err := client.load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "head", loaded.PR.Head.SHA)
	count := 0
	for _, row := range loaded.Rows {
		if row.Kind == commentRow {
			count++
		}
	}

	assert.Equal(t, 303, count, "all pages of inline comments, general comments and reviews")
	for _, withComments := range []bool{false, true} {
		t.Run(fmt.Sprintf("comments=%t", withComments), func(t *testing.T) {
			request := reviewRequest{CommitID: "head", Event: "APPROVE"}
			if withComments {
				request.Comments = []inlineComment{{anchor: anchor{Path: "example.go", Side: "LEFT", Line: 6}, Body: "check this"}}
			}

			sent, subErr := client.submitReview(context.Background(), loaded.PR, request)
			require.NoError(t, subErr)
			assert.True(t, sent)
			if difference := cmp.Diff(request, submitted[len(submitted)-1], cmp.AllowUnexported(inlineComment{})); difference != "" {
				t.Errorf("submitted request mismatch (-expected +actual):\n%s", difference)
			}

			assert.Equal(t, []string{"GET /repos/owner/repo/pulls/123", "POST /repos/owner/repo/pulls/123/reviews"}, calls[len(calls)-2:])
		})
	}

	assert.Len(t, submitted, 2, "one POST per approval")
}

func TestCommentsOnlySubmission(t *testing.T) {
	t.Parallel()
	var submitted reviewRequest
	posts := 0
	client := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			require.NoError(t, json.NewDecoder(r.Body).Decode(&submitted))
			fmt.Fprint(w, `{"state":"COMMENTED"}`)
			return
		}

		fmt.Fprint(w, `{"state":"open","head":{"sha":"head"},"base":{"sha":"base"}}`)
	})
	m := newModel(context.Background(), client)
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	m, command := press(t, m, 's', 0)
	assert.Nil(t, command, "nothing to submit without local comments")
	assert.Zero(t, posts)
	index := codeIndex(t, m.snapshot.Rows, "+added")
	m.comments[index] = "This also works on my own PR."
	m, command = press(t, m, 's', 0)
	require.NotNil(t, command)
	assert.Equal(t, submitting, m.activity)
	message := command().(submittedMsg)
	require.NoError(t, message.Err)
	assert.Equal(t, commentEvent, submitted.Event)
	assert.Equal(t, "head", submitted.CommitID)
	assert.Equal(t, "Inline review comments.", submitted.Body)
	require.Len(t, submitted.Comments, 1)
	assert.Equal(t, m.comments[index], submitted.Comments[0].Body)
	assert.Equal(t, 1, posts)
	updated, quit := m.Update(message)
	assert.Equal(t, commentEvent, updated.(model).submittedEvent)
	require.NotNil(t, quit)
}

func TestSDKRefusesChangedHead(t *testing.T) {
	t.Parallel()
	client := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method, "must not submit a review")
		fmt.Fprint(w, `{"state":"open","head":{"sha":"new"},"base":{"sha":"base"}}`)
	})
	previous := parsedSnapshot(t, sampleDiff, "old")
	sent, err := client.submitReview(context.Background(), previous.PR, reviewRequest{CommitID: "old", Event: approvalEvent})
	require.ErrorIs(t, err, errHeadChanged)
	assert.False(t, sent)
	sent, err = client.submitReview(context.Background(), previous.PR, reviewRequest{CommitID: "old", Event: commentEvent, Body: "Inline review comments."})
	require.ErrorIs(t, err, errHeadChanged)
	assert.False(t, sent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.load(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestSDKDetectsChangesDuringLoad(t *testing.T) {
	t.Parallel()
	metadataCalls := 0
	client := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Accept") == "application/vnd.github.v3.diff":
			fmt.Fprint(w, sampleDiff)
		case r.URL.RawQuery != "":
			fmt.Fprint(w, "[]")
		default:
			metadataCalls++
			fmt.Fprintf(w, `{"state":"open","head":{"sha":"head%d"},"base":{"sha":"base"}}`, metadataCalls)
		}
	})
	_, err := client.load(context.Background())
	assert.ErrorIs(t, err, errHeadChanged)
}

func TestSDKFailureKeepsLocalComments(t *testing.T) {
	t.Parallel()
	client := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed"}`)
			return
		}

		fmt.Fprint(w, `{"state":"open","head":{"sha":"head"},"base":{"sha":"base"}}`)
	})
	m := newModel(context.Background(), client)
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	index := codeIndex(t, m.snapshot.Rows, "+added")
	m.comments[index] = "keep me"
	sent, err := client.submitReview(context.Background(), m.snapshot.PR, m.request(approvalEvent))
	require.Error(t, err)
	var apiErr *api.HTTPError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnprocessableEntity, apiErr.StatusCode)
	updated, _ := m.Update(submittedMsg{Sent: sent, Err: err})
	assert.Equal(t, "keep me", updated.(model).comments[index])
}

func runSearch(t *testing.T, m model, pattern string) model {
	t.Helper()
	m, _ = press(t, m, '/', 0)
	require.Equal(t, enteringSearch, m.activity)
	m.searchInput.SetValue(pattern)
	m, command := press(t, m, tea.KeyEnter, 0)
	require.NotNil(t, command)
	require.Equal(t, findingSearch, m.activity)
	batch, ok := command().(tea.BatchMsg)
	require.True(t, ok)
	found := false
	for _, subcommand := range batch {
		if result, ok := subcommand().(searchResultsMsg); ok {
			require.NoError(t, result.Err)
			updated, _ := m.Update(result)
			m = updated.(model)
			found = true
		}
	}

	require.True(t, found)
	return m
}

func TestRegexSearchNavigation(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 12})
	m = updated.(model)
	m = runSearch(t, m, "^(added|other|gone)$")
	require.Len(t, m.matches, 3)
	assert.Equal(t, codeIndex(t, m.snapshot.Rows, "+added"), m.selected)
	m, _ = press(t, m, 'n', 0)
	assert.Equal(t, codeIndex(t, m.snapshot.Rows, " other"), m.selected)
	m, _ = press(t, m, 'n', 0)
	assert.Equal(t, codeIndex(t, m.snapshot.Rows, "-gone"), m.selected)
	assert.Greater(t, m.viewport.YOffset(), 0)
	m, _ = press(t, m, 'n', 0)
	assert.Equal(t, codeIndex(t, m.snapshot.Rows, "+added"), m.selected, "wrap forwards")
	m, _ = press(t, m, 'p', 0)
	assert.Equal(t, codeIndex(t, m.snapshot.Rows, "-gone"), m.selected, "wrap backwards")
	m, _ = press(t, m, 'p', 0)
	assert.Equal(t, codeIndex(t, m.snapshot.Rows, " other"), m.selected)
	assert.Contains(t, m.notice, "2/3")
	assert.True(t, m.rowStyle(m.selected).GetUnderline())
	assert.NotContains(t, m.gutter(viewport.GutterContext{Index: m.selected}), "›")
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "new-head"))
	assert.Empty(t, m.matches, "refresh cannot keep stale match indices")
}

func TestSearchErrorsAndCancellation(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	m = runSearch(t, m, "added")
	m, _ = press(t, m, '/', 0)
	m.searchInput.SetValue("[")
	m, command := press(t, m, tea.KeyEnter, 0)
	assert.Nil(t, command)
	assert.Equal(t, enteringSearch, m.activity)
	assert.Contains(t, m.notice, "Invalid regex")
	require.Len(t, m.matches, 1, "invalid input keeps the previous search")
	m, _ = press(t, m, tea.KeyEscape, 0)
	assert.Equal(t, browsing, m.activity)
	assert.Empty(t, m.searchPattern)
	assert.Empty(t, m.matches)
	m = runSearch(t, m, "no-such-line")
	assert.Empty(t, m.matches)
	assert.Contains(t, m.notice, "No matching lines")
	m, _ = press(t, m, 'n', 0)
	assert.Equal(t, browsing, m.activity)
	m, _ = press(t, m, '/', 0)
	m.searchInput.SetValue("")
	m, command = press(t, m, tea.KeyEnter, 0)
	assert.Nil(t, command)
	assert.Empty(t, m.searchPattern)
	assert.Empty(t, m.matches)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := findMatches(ctx, m.snapshot.Rows, regexp.MustCompile("added"))
	assert.ErrorIs(t, err, context.Canceled)
}

func TestSearchScrollsToLongLineMatch(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	snapshot := parsedSnapshot(t, sampleDiff, "head")
	index := codeIndex(t, snapshot.Rows, "+added")
	snapshot.Rows[index].Text = fmt.Sprintf("+%sneedle", strings.Repeat("α\t", 100))
	setSnapshot(t, &m, snapshot)
	m.activity = browsing
	m = runSearch(t, m, "needle$")
	require.Len(t, m.matches, 1)
	assert.Equal(t, index, m.selected)
	assert.Equal(t, 515, m.matches[0].Column)
	assert.Greater(t, m.viewport.XOffset(), 0)
	assert.Contains(t, ansi.Strip(m.View().Content), "needle")
}

func TestSearchCaseSensitivity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		pattern string
		count   int
	}{
		{pattern: "^ADDED$", count: 1},
		{pattern: "(?-i)^ADDED$", count: 0},
		{pattern: "(?-i)^added$", count: 1},
	} {
		t.Run(test.pattern, func(t *testing.T) {
			t.Parallel()
			m := newModel(t.Context(), githubPR{})
			setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
			m.activity = browsing
			m = runSearch(t, m, test.pattern)
			assert.Len(t, m.matches, test.count)
		})
	}
}

func TestEscapeClearsSearch(t *testing.T) {
	t.Parallel()
	m := newModel(t.Context(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	m = runSearch(t, m, "added")
	selected := m.selected
	m, _ = press(t, m, tea.KeyEscape, 0)
	assert.Empty(t, m.searchPattern)
	assert.Empty(t, m.searchInput.Value())
	assert.Empty(t, m.matches)
	assert.False(t, m.rowStyle(selected).GetUnderline())
	m, _ = press(t, m, 'n', 0)
	assert.Equal(t, selected, m.selected)
	m, _ = press(t, m, 'p', 0)
	assert.Equal(t, selected, m.selected)

	m, _ = press(t, m, '/', 0)
	m.searchInput.SetValue("other")
	m, command := press(t, m, tea.KeyEnter, 0)
	require.NotNil(t, command)
	m, _ = press(t, m, tea.KeyEscape, 0)
	batch, ok := command().(tea.BatchMsg)
	require.True(t, ok)
	for _, subcommand := range batch {
		if result, ok := subcommand().(searchResultsMsg); ok {
			updated, _ := m.Update(result)
			m = updated.(model)
		}
	}

	assert.Equal(t, browsing, m.activity)
	assert.Empty(t, m.matches, "a cancelled search must not restore highlights")
	assert.Empty(t, m.searchPattern)
	assert.Equal(t, selected, m.selected)
}

func TestEditorEnterAndPlaceholder(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	m.selected = codeIndex(t, m.snapshot.Rows, "+added")
	m, _ = press(t, m, 'c', 0)
	assert.Contains(t, ansi.Strip(m.commentModal()), "Write an inline comment")
	styles := m.editor.Styles()
	assert.NotEqual(t, styles.Focused.Placeholder.GetForeground(), styles.Focused.Base.GetBackground())
	assert.Equal(t, styles.Focused.Base.GetBackground(), styles.Focused.CursorLine.GetBackground())
	m.editor.SetValue("first")
	m, _ = press(t, m, tea.KeyEnter, tea.ModShift)
	assert.Equal(t, editing, m.activity)
	assert.Equal(t, "first\n", m.editor.Value())
	m.editor.InsertString("second")
	m, _ = press(t, m, tea.KeyEnter, 0)
	assert.Equal(t, browsing, m.activity)
	assert.Equal(t, "first\nsecond", m.comments[m.selected])
}

func TestModalPaintsEveryInteriorCell(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "Hello", "First\n\nThird"} {
		t.Run(fmt.Sprintf("text=%q", value), func(t *testing.T) {
			t.Parallel()
			m := newModel(context.Background(), githubPR{})
			setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
			m.activity = browsing
			m.selected = codeIndex(t, m.snapshot.Rows, "+added")
			m, _ = press(t, m, 'c', 0)
			m.editor.SetValue("Erase this text before checking the placeholder")
			m.editor.SetValue(value)
			modal := m.commentModal()
			width, height := lipgloss.Size(modal)
			canvas := lipgloss.NewCanvas(width, height).Compose(lipgloss.NewCompositor(lipgloss.NewLayer(modal)))
			missing, different := 0, 0
			expected := color.NRGBAModel.Convert(lipgloss.Color(modalBackground))
			for y := 1; y < height-1; y++ {
				for x := 1; x < width-1; x++ {
					cell := canvas.CellAt(x, y)
					if cell == nil || cell.Style.Bg == nil {
						missing++
						continue
					}

					if color.NRGBAModel.Convert(cell.Style.Bg) != expected {
						different++
					}
				}
			}

			assert.Zero(t, missing, "modal cells must not reveal the terminal background")
			assert.Zero(t, different, "modal background must stay uniform")
		})
	}
}

func TestInitialLoadingScreen(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{Repo: "owner/repo", Number: 123})
	content := ansi.Strip(m.View().Content)
	assert.Contains(t, content, "Loading PR")
	assert.Contains(t, content, "owner/repo #123")
	assert.NotContains(t, content, "j k move")
	assert.NotContains(t, content, "No diff lines")
	updated, command := m.Update(m.spinner.Tick())
	require.NotNil(t, command, "spinner continues ticking during loading")
	m = updated.(model)
	updated, _ = m.Update(loadedMsg{ID: m.loadID, Err: errors.New("fetch failed")})
	m = updated.(model)
	content = ansi.Strip(m.View().Content)
	assert.Contains(t, content, "fetch failed")
	assert.NotContains(t, content, "j k move")
	m, command = press(t, m, 'r', 0)
	require.NotNil(t, command)
	assert.Equal(t, loading, m.activity)
	updated, _ = m.Update(loadResult(t, m, parsedSnapshot(t, sampleDiff, "head")))
	m = updated.(model)
	content = ansi.Strip(m.View().Content)
	assert.Contains(t, content, "example.go")
	assert.Contains(t, content, "j k move")
	_, command = m.Update(m.spinner.Tick())
	assert.Nil(t, command, "spinner stops after loading")
}

func TestModalHasOneSetOfHints(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	m.selected = codeIndex(t, m.snapshot.Rows, "+added")
	m, _ = press(t, m, 'c', 0)
	content := ansi.Strip(m.View().Content)
	assert.Equal(t, 1, strings.Count(content, "enter save"))
	assert.NotContains(t, content, "mouse wheel scrolls")
	assert.NotContains(t, content, "j k move")
	m, _ = press(t, m, 's', tea.ModCtrl)
	content = ansi.Strip(m.View().Content)
	assert.Contains(t, content, "Comment is empty")
	assert.Equal(t, 1, strings.Count(content, "enter save"))
}

func TestEmptyDiffView(t *testing.T) {
	t.Parallel()
	m := newModel(context.Background(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, "", "head"))
	m.activity = browsing
	assert.Contains(t, m.View().Content, "No diff lines")
}

func BenchmarkLargeDiffScroll(b *testing.B) {
	for _, count := range []int{1000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			m := newModel(context.Background(), githubPR{})
			m.activity = browsing
			m.snapshot.PR.Head.SHA = "head"
			m.snapshot.Rows = append(m.snapshot.Rows, diffRow{Kind: fileRow, Text: "large.go"})
			for i := range count {
				m.snapshot.Rows = append(m.snapshot.Rows, diffRow{Kind: codeRow, Text: " unchanged source code", anchor: anchor{Path: "large.go", Side: "RIGHT", Line: i + 1}, NewLine: i + 1, OldLine: i + 1})
			}

			m.selected = count / 2
			setSnapshot(b, &m, m.snapshot)
			m.viewport.SetYOffset(m.selected)
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				wheel := tea.MouseWheelMsg{Button: tea.MouseWheelDown}
				if i%2 == 1 {
					wheel.Button = tea.MouseWheelUp
				}

				updated, _ := m.Update(wheel)
				m = updated.(model)
				m.View()
			}
		})
	}
}

func TestRefreshCannotUnlockSubmission(t *testing.T) {
	t.Parallel()
	m := newModel(t.Context(), githubPR{})
	setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
	m.activity = browsing
	m.comments[codeIndex(t, m.snapshot.Rows, "+added")] = "local comment"
	m, _ = press(t, m, 'r', 0)
	result := loadResult(t, m, m.snapshot)
	m, _ = press(t, m, 'q', 0)
	require.Equal(t, confirmQuit, m.activity)
	m, _ = press(t, m, 'n', 0)
	require.Equal(t, loading, m.activity, "cancelling quit must restore the pending refresh")
	m, command := press(t, m, 'a', 0)
	assert.Nil(t, command, "submission stays locked until refresh finishes")
	updated, _ := m.Update(result)
	m = updated.(model)
	require.Equal(t, browsing, m.activity)
	m, command = press(t, m, 'a', 0)
	require.NotNil(t, command)
	require.Equal(t, submitting, m.activity)
	updated, _ = m.Update(result)
	m = updated.(model)
	assert.Equal(t, submitting, m.activity, "late load results cannot unlock submission")
	m, command = press(t, m, 'a', 0)
	assert.Nil(t, command, "a second submission must not be scheduled")
}

func TestRefreshCompletesDuringQuitConfirmation(t *testing.T) {
	t.Parallel()
	for _, loadErr := range []error{nil, errors.New("load failed")} {
		t.Run(fmt.Sprint(loadErr), func(t *testing.T) {
			t.Parallel()
			m := newModel(t.Context(), githubPR{})
			setSnapshot(t, &m, parsedSnapshot(t, sampleDiff, "head"))
			m.activity = browsing
			m.comments[codeIndex(t, m.snapshot.Rows, "+added")] = "saved"
			m, _ = press(t, m, 'r', 0)
			result := loadResult(t, m, m.snapshot)
			result.Err = loadErr
			m, _ = press(t, m, 'q', 0)
			updated, _ := m.Update(result)
			m = updated.(model)
			assert.Equal(t, confirmQuit, m.activity)
			assert.Contains(t, m.notice, "Discard local comments")
			m, _ = press(t, m, 'n', 0)
			assert.Equal(t, browsing, m.activity)
			assert.Len(t, m.comments, 1)
			if loadErr != nil {
				assert.Contains(t, m.notice, "Load failed")
			}
		})
	}
}

func TestRefreshIgnoresOldResultsAndUsesCurrentSize(t *testing.T) {
	t.Parallel()
	m := newModel(t.Context(), githubPR{})
	result := loadResult(t, m, parsedSnapshot(t, sampleDiff, "head"))
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 63, Height: 19})
	m = updated.(model)
	stale := result
	stale.ID--
	updated, _ = m.Update(stale)
	m = updated.(model)
	assert.Equal(t, loading, m.activity)
	assert.Empty(t, m.snapshot.Rows)
	updated, _ = m.Update(result)
	m = updated.(model)
	assert.Equal(t, 61, m.viewport.Width())
	assert.Equal(t, 15, m.viewport.Height())
	assert.Equal(t, 63, lipgloss.Width(m.View().Content))
	assert.Equal(t, 19, lipgloss.Height(m.View().Content))
}

func TestIndexedClippingMatchesUnicodeClipping(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"short text",
		strings.Repeat("x", 10000),
		strings.Repeat("界aé e\u0301 🦊 ", 1000),
		fmt.Sprint(strings.Repeat("a", 255), "界", strings.Repeat("z", 1000)),
	} {
		line, err := indexDisplayLine(t.Context(), text)
		require.NoError(t, err)
		for _, left := range []int{0, 1, 254, 255, 256, 257, 500, 9900, 20000} {
			for _, width := range []int{0, 1, 2, 80, 300} {
				assert.Equal(t, ansi.Cut(text, left, left+width), line.cut(left, left+width), "left=%d width=%d", left, width)
			}
		}
	}
}

func TestLongLineCanScrollToItsFinalCharacters(t *testing.T) {
	t.Parallel()
	m := newModel(t.Context(), githubPR{})
	next := parsedSnapshot(t, sampleDiff, "head")
	index := codeIndex(t, next.Rows, "+added")
	next.Rows[index].Text = fmt.Sprintf("+%sEND", strings.Repeat("界", 10000))
	setSnapshot(t, &m, next)
	m.activity, m.selected = browsing, index
	m.viewport.SetXOffset(1000000)
	assert.Contains(t, ansi.Strip(m.View().Content), "END")
	assert.Equal(t, m.width, lipgloss.Width(m.View().Content))
}

func TestPreparationHonoursCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := prepareSnapshot(ctx, snapshot{})
	assert.ErrorIs(t, err, context.Canceled)
	_, err = retainComments(ctx, snapshot{}, snapshot{}, nil)
	assert.ErrorIs(t, err, context.Canceled)
	_, _, err = indexAnchors(ctx, nil, nil)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = indexDisplayLine(ctx, "text")
	assert.ErrorIs(t, err, context.Canceled)
}

func benchmarkSnapshot(count, width int) snapshot {
	result := snapshot{}
	result.PR.Head.SHA, result.PR.Base.SHA = "head", "base"
	for i := range count {
		result.Rows = append(result.Rows, diffRow{
			Kind: codeRow, Text: fmt.Sprintf(" %s", strings.Repeat("x", width)),
			anchor:  anchor{Path: "large.go", Side: "RIGHT", Line: i + 1},
			OldLine: i + 1, NewLine: i + 1,
		})
	}

	return result
}

func BenchmarkLongLineScroll(b *testing.B) {
	for _, width := range []int{80, 10000, 100000} {
		for _, column := range []int{0, width / 2} {
			b.Run(fmt.Sprintf("width=%d/column=%d", width, column), func(b *testing.B) {
				m := newModel(b.Context(), githubPR{})
				setSnapshot(b, &m, benchmarkSnapshot(40, width))
				m.activity = browsing
				m.viewport.SetXOffset(column)
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					button := tea.MouseWheelDown
					if i%2 == 1 {
						button = tea.MouseWheelUp
					}

					updated, _ := m.Update(tea.MouseWheelMsg{Button: button})
					m = updated.(model)
					m.View()
				}
			})
		}
	}
}

func BenchmarkRepeatedAnchorRefresh(b *testing.B) {
	for _, count := range []int{1, 50} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			previous := benchmarkSnapshot(10000, 40)
			next := previous
			next.PR.Head.SHA = "new"
			comments := make(map[int]string)
			for i := range count {
				comments[3+i*10] = "comment on repeated text"
			}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_, err := retainComments(b.Context(), previous, next, comments)
				require.NoError(b, err)
			}
		})
	}
}

func BenchmarkLoadCompletion(b *testing.B) {
	for _, count := range []int{1000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			m := newModel(b.Context(), githubPR{})
			result := loadResult(b, m, benchmarkSnapshot(count, 80))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.Update(result)
			}
		})
	}
}

func TestTerminalFlow(t *testing.T) {
	var submitted reviewRequest
	client := testGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			if err := json.NewDecoder(r.Body).Decode(&submitted); err != nil {
				t.Error(err)
			}

			fmt.Fprint(w, `{"state":"APPROVED"}`)
		case r.Header.Get("Accept") == "application/vnd.github.v3.diff":
			fmt.Fprint(w, sampleDiff)
		case r.URL.RawQuery != "":
			fmt.Fprint(w, "[]")
		default:
			fmt.Fprint(w, `{"state":"open","head":{"sha":"head"},"base":{"sha":"base"}}`)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	loaded := make(chan struct{}, 2)
	program := tea.NewProgram(newModel(ctx, client), tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
		if _, ok := msg.(loadedMsg); ok {
			loaded <- struct{}{}
		}

		return msg
	}))
	go func() {
		select {
		case <-loaded:
		case <-ctx.Done():
			return
		}

		for _, key := range []tea.KeyPressMsg{
			{Code: 'j'}, {Code: 'j'}, {Code: 'c'},
			{Text: "first"}, {Code: tea.KeyEnter, Mod: tea.ModShift}, {Text: "second"},
			{Code: tea.KeyEnter}, {Code: 'r'},
		} {
			program.Send(key)
		}

		select {
		case <-loaded:
			program.Send(tea.KeyPressMsg{Code: 'a'})
		case <-ctx.Done():
		}
	}()
	result, err := program.Run()
	require.NoError(t, err)
	assert.Equal(t, approvalEvent, result.(model).submittedEvent)
	require.Len(t, submitted.Comments, 1)
	assert.Equal(t, "first\nsecond", submitted.Comments[0].Body)
	assert.Equal(t, "RIGHT", submitted.Comments[0].Side)
	assert.Equal(t, 5, submitted.Comments[0].Line)
}
