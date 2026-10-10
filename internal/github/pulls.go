package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	gh "github.com/google/go-github/v92/github"
)

// PullRequest gives the pull request number.
func (r Repository) PullRequest(ctx context.Context, number int64) (*gh.PullRequest, error) {
	pullRequest, _, err := r.Client.PullRequests.Get(ctx, r.Owner(), r.Name(), int(number))
	return pullRequest, err
}

// CreateDraftPullRequest opens a draft pull request from the branch head into the branch base.
func (r Repository) CreateDraftPullRequest(ctx context.Context, title, head, base, body string) (*gh.PullRequest, error) {
	pullRequest, _, err := r.Client.PullRequests.Create(ctx, r.Owner(), r.Name(), gh.CreatePullRequest{
		Title: &title,
		Head:  head,
		Base:  base,
		Body:  &body,
		Draft: new(true),
	})
	return pullRequest, err
}

// MarkReadyForReview makes the draft pull request with the GraphQL node id ready for review.
func (r Repository) MarkReadyForReview(ctx context.Context, id string) error {
	const query = `mutation($id: ID!) { markPullRequestReadyForReview(input: { pullRequestId: $id }) { clientMutationId } }`
	var data any
	return r.graphql(ctx, query, map[string]any{"id": id}, &data)
}

// ConvertToDraft makes the pull request with the GraphQL node id a draft.
func (r Repository) ConvertToDraft(ctx context.Context, id string) error {
	const query = `mutation($id: ID!) { convertPullRequestToDraft(input: { pullRequestId: $id }) { clientMutationId } }`
	var data any
	return r.graphql(ctx, query, map[string]any{"id": id}, &data)
}

// InlineComment is a comment of a new review on a line of a file.
type InlineComment struct {
	// Path is relative to the root of the repository.
	Path string `json:"path"`
	// Line is a line of the new version of the file. It must be in the diff.
	Line int    `json:"line"`
	Body string `json:"body"`
}

// SubmitReview posts a review of the commit of the pull request number with body and the inline comments. The
// review approves nothing and requests no change.
func (r Repository) SubmitReview(ctx context.Context, number int64, commit, body string, comments []InlineComment) error {
	drafts := make([]*gh.DraftReviewComment, 0, len(comments))
	for _, comment := range comments {
		drafts = append(drafts, &gh.DraftReviewComment{Path: &comment.Path, Line: &comment.Line, Body: &comment.Body})
	}
	_, _, err := r.Client.PullRequests.CreateReview(ctx, r.Owner(), r.Name(), int(number), &gh.PullRequestReviewRequest{
		CommitID: &commit,
		Body:     &body,
		Event:    new("COMMENT"),
		Comments: drafts,
	})
	return err
}

// CreateCheckRun adds the check run name with status to the commit sha. A completed check run has the conclusion. A
// check run with a title has the title and the summary.
func (r Repository) CreateCheckRun(ctx context.Context, name, sha, status, conclusion, title, summary string) error {
	options := gh.CreateCheckRunOptions{Name: name, HeadSHA: sha, Status: &status}
	if conclusion != "" {
		options.Conclusion = &conclusion
	}
	if title != "" {
		options.Output = &gh.CheckRunOutput{Title: &title, Summary: &summary}
	}
	_, _, err := r.Client.Checks.CreateCheckRun(ctx, r.Owner(), r.Name(), options)
	return err
}

// UpdateCheckRun sets the status of the check run id of name. A completed check run has the conclusion. A check run
// with a title gets the title and the summary.
func (r Repository) UpdateCheckRun(ctx context.Context, id int64, name, status, conclusion, title, summary string) error {
	options := gh.UpdateCheckRunOptions{Name: name, Status: &status}
	if conclusion != "" {
		options.Conclusion = &conclusion
	}
	if title != "" {
		options.Output = &gh.CheckRunOutput{Title: &title, Summary: &summary}
	}
	_, _, err := r.Client.Checks.UpdateCheckRun(ctx, r.Owner(), r.Name(), id, options)
	return err
}

// CompleteCheckRun completes the check run id of name with conclusion, for example success.
func (r Repository) CompleteCheckRun(ctx context.Context, id int64, name, conclusion string) error {
	_, _, err := r.Client.Checks.UpdateCheckRun(ctx, r.Owner(), r.Name(), id, gh.UpdateCheckRunOptions{Name: name, Status: new("completed"), Conclusion: &conclusion})
	return err
}

// CheckRuns gives the check runs of the commit sha.
func (r Repository) CheckRuns(ctx context.Context, sha string) ([]*gh.CheckRun, error) {
	return all(r.Client.Checks.ListCheckRunsForRefIter(ctx, r.Owner(), r.Name(), sha, &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}))
}

// WorkflowRuns gives the workflow runs of GitHub Actions on the commit sha.
func (r Repository) WorkflowRuns(ctx context.Context, sha string) ([]*gh.WorkflowRun, error) {
	return all(r.Client.Actions.ListRepositoryWorkflowRunsIter(ctx, r.Owner(), r.Name(), &gh.ListWorkflowRunsOptions{HeadSHA: sha, ListOptions: gh.ListOptions{PerPage: 100}}))
}

// CheckRunAnnotations gives the annotations of the check run id.
func (r Repository) CheckRunAnnotations(ctx context.Context, id int64) ([]*gh.CheckRunAnnotation, error) {
	return all(r.Client.Checks.ListCheckRunAnnotationsIter(ctx, r.Owner(), r.Name(), id, &gh.ListOptions{PerPage: 100}))
}

// JobLog gives the log of the job of GitHub Actions with the id. The job of a check run of GitHub Actions has the id
// of the check run. The log comes from a link of GitHub that needs no token, so the token does not go to that host.
func (r Repository) JobLog(ctx context.Context, id int64) (string, error) {
	link, _, err := r.Client.Actions.GetWorkflowJobLogs(ctx, r.Owner(), r.Name(), id, 1)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, link.String(), http.NoBody)
	if err != nil {
		return "", err
	}
	response, err := (&http.Client{Timeout: Timeout}).Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the job log of %d: %s", id, response.Status)
	}
	text, err := io.ReadAll(response.Body)
	return string(text), err
}

// MergePullRequest squash merges the pull request number if its head is sha. It tells if GitHub merged the pull
// request. It gives the reason when GitHub refuses the merge, for example for a rule of the base branch. A head that is
// not sha is no refusal and no merge, because the next poll reads the new head.
func (r Repository) MergePullRequest(ctx context.Context, number int64, sha string) (bool, string, error) {
	_, _, err := r.Client.PullRequests.Merge(ctx, r.Owner(), r.Name(), int(number), "", &gh.PullRequestOptions{MergeMethod: "squash", SHA: sha})
	var response *gh.ErrorResponse
	if errors.As(err, &response) && response.Response.StatusCode == http.StatusConflict {
		return false, "", nil
	}
	if errors.As(err, &response) && (response.Response.StatusCode == http.StatusMethodNotAllowed || response.Response.StatusCode == http.StatusUnprocessableEntity) {
		return false, response.Message, nil
	}
	return err == nil, "", err
}

// UserID gives the id of the account login.
func (r Repository) UserID(ctx context.Context, login string) (int64, error) {
	user, _, err := r.Client.Users.Get(ctx, login)
	return user.GetID(), err
}
