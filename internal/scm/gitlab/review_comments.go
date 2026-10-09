package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

var _ scm.ReviewCommentsHost = (*Host)(nil)

// gitlabUser is the author object GitLab attaches to a note.
type gitlabUser struct {
	Username string `json:"username"`
}

// gitlabNotePosition is the diff position of a resolvable note. A note on an
// added line carries new_path/new_line; one on a removed or unchanged line
// carries old_path/old_line instead, which is why both halves are decoded.
type gitlabNotePosition struct {
	NewPath string `json:"new_path"`
	NewLine *int   `json:"new_line"`
	OldPath string `json:"old_path"`
	OldLine *int   `json:"old_line"`
}

// gitlabNote is one note of a merge request discussion.
type gitlabNote struct {
	ID         int                 `json:"id"`
	Body       string              `json:"body"`
	System     bool                `json:"system"`
	Resolvable bool                `json:"resolvable"`
	Resolved   bool                `json:"resolved"`
	CreatedAt  time.Time           `json:"created_at"`
	Author     *gitlabUser         `json:"author"`
	Position   *gitlabNotePosition `json:"position"`
}

// gitlabDiscussion is one merge request discussion as
// `projects/:id/merge_requests/:iid/discussions` returns it.
type gitlabDiscussion struct {
	ID    string       `json:"id"`
	Notes []gitlabNote `json:"notes"`
}

// positionPathLine returns the file and line the note is anchored to. An
// added-line comment reports the new side; a removed-line comment has no
// new_line, so the old side is the only anchor available.
func (n gitlabNote) positionPathLine() (string, int) {
	if n.Position == nil {
		return "", 0
	}
	if n.Position.NewLine != nil {
		return strings.TrimSpace(n.Position.NewPath), *n.Position.NewLine
	}
	if n.Position.OldLine != nil {
		return strings.TrimSpace(n.Position.OldPath), *n.Position.OldLine
	}
	path := strings.TrimSpace(n.Position.NewPath)
	if path == "" {
		path = strings.TrimSpace(n.Position.OldPath)
	}
	return path, 0
}

// ProjectPathFromMRURL returns the "group/project" path a GitLab merge request
// URL addresses, or "" when raw is not one. ProjectPath is its repository-remote
// sibling, and both are needed: a reattached run can have a PR URL whose project
// differs from the resolved upstream remote.
func ProjectPathFromMRURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	segments, _, ok := mergeRequestURLPath(parsed)
	if !ok {
		return ""
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return ""
		}
	}
	return strings.Join(segments, "/")
}

// GetReviewComments implements scm.ReviewCommentsHost. It reads the merge
// request's unresolved discussion notes, which is GitLab's equivalent of
// GitHub's unresolved review threads.
//
// Two GitLab facts shape the filter. Resolution is reported on a discussion's
// notes, and only a diff note is resolvable at all: a general MR comment
// reports resolvable=false and can never be a review finding. And a bot posts
// as an ordinary account here - there are no GitHub App slugs and no "[bot]"
// login spelling - so identity comes from the registered review-bot logins
// (scm.IsReviewBotLogin), never from an application name.
func (h *Host) GetReviewComments(ctx context.Context, pr *scm.PR) ([]scm.ReviewComment, error) {
	project, iid, err := h.reviewCommentTarget(pr)
	if err != nil {
		return nil, err
	}
	// --paginate walks every page; a merge request with more discussions than
	// fit on one page (GitLab defaults to 20 per page) would otherwise silently
	// drop the later ones and an unresolved bot comment would never become a
	// finding. glab writes the pages as one JSON array (json output) or as
	// concatenated arrays, both of which decodeGitlabDiscussions accepts.
	args := []string{
		"api", "--paginate",
		fmt.Sprintf("projects/%s/merge_requests/%d/discussions?per_page=100", encodeProjectPath(project), iid),
	}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	out, err := h.cmd(ctx, "glab", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("glab api MR discussions: %s: %w", strings.TrimSpace(string(out)), err)
	}
	discussions, err := decodeGitlabDiscussions(out)
	if err != nil {
		return nil, err
	}
	var comments []scm.ReviewComment
	for _, discussion := range discussions {
		for _, note := range discussion.Notes {
			if !note.Resolvable || note.Resolved || note.System {
				continue
			}
			if note.Author == nil || !scm.IsReviewBotLogin(scm.ProviderGitLab, note.Author.Username) {
				continue
			}
			path, line := note.positionPathLine()
			id := ""
			if note.ID != 0 {
				id = strconv.Itoa(note.ID)
			}
			comments = append(comments, scm.ReviewComment{
				ID:        id,
				Author:    note.Author.Username,
				Path:      path,
				Line:      line,
				Body:      note.Body,
				CreatedAt: note.CreatedAt,
				URL:       reviewCommentURL(pr, note.ID),
			})
		}
	}
	return comments, nil
}

// reviewCommentURL builds the merge request link that lands on the note. The
// discussions API returns no per-note URL, and the MR web URL plus the note
// anchor is the link GitLab itself renders.
func reviewCommentURL(pr *scm.PR, noteID int) string {
	if pr == nil || noteID == 0 {
		return ""
	}
	base := strings.TrimSpace(pr.URL)
	if base == "" {
		return ""
	}
	return base + "#note_" + strconv.Itoa(noteID)
}

// reviewCommentTarget resolves the project path and merge request IID the
// discussions read addresses. A host built without a project path (or a run
// whose remote could not be parsed) falls back to the recorded PR URL, which is
// validated against the host and project exactly like every other MR-URL read.
//
// It reports scm.ErrUnsupported when neither source names a project: the host
// was built without the coordinates this read needs, which is the same
// "provider cannot supply comments" answer a provider without the capability
// gives, not a failed read to park on.
func (h *Host) reviewCommentTarget(pr *scm.PR) (string, int, error) {
	if pr == nil {
		return "", 0, errors.New("pr is nil")
	}
	project := strings.Trim(strings.TrimSpace(h.projectPath), "/")
	number := 0
	if parsed, err := strconv.Atoi(strings.TrimSpace(pr.Number)); err == nil && parsed > 0 {
		number = parsed
	}
	if pr.URL != "" {
		parsed, err := parseMergeRequestURL(pr.URL, h.host, project)
		if err != nil {
			return "", 0, err
		}
		if number == 0 {
			number = parsed
		}
		if project == "" {
			project = ProjectPathFromMRURL(pr.URL)
		}
	}
	if number <= 0 {
		return "", 0, fmt.Errorf("%w: cannot determine the GitLab merge request for review comments", scm.ErrUnsupported)
	}
	if project == "" {
		return "", 0, fmt.Errorf("%w: cannot determine the GitLab project path for review comments", scm.ErrUnsupported)
	}
	return project, number, nil
}

// decodeGitlabDiscussions reads every discussion from glab output. The payload
// is one JSON array (the default json output merges the pages) or several
// arrays concatenated back to back, one per page. A streaming decoder reads
// each top-level value in turn. A malformed document is an error rather than an
// empty result: an unreadable page must never look like "no unresolved
// comments".
func decodeGitlabDiscussions(out []byte) ([]gitlabDiscussion, error) {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, errors.New("decode gitlab discussions: expected a discussion array")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var discussions []gitlabDiscussion
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return discussions, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode gitlab discussions: %w", err)
		}
		var page []gitlabDiscussion
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode gitlab discussions: %w", err)
		}
		if page == nil {
			return nil, errors.New("decode gitlab discussions: expected a discussion array")
		}
		for _, discussion := range page {
			if discussion.ID == "" || discussion.Notes == nil {
				return nil, errors.New("decode gitlab discussions: expected a discussion with id and notes array")
			}
		}
		discussions = append(discussions, page...)
	}
}
