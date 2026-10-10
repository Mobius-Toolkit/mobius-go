package testkit

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// PullRequest is a pull request that the fake GitHub opened.
type PullRequest struct {
	Number int64
	Title  string
	Body   string
	Head   string
	Base   string
	Draft  bool
}

// CheckRun is a check run of a commit.
type CheckRun struct {
	Name    string
	HeadSHA string
	Status  string
	// Conclusion is "" while the check run runs.
	Conclusion string
	Output     *CheckRunOutput
}

// CheckRunOutput is the title and the summary of a check run.
type CheckRunOutput struct {
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// WorkflowRun is a workflow run of GitHub Actions on a commit.
type WorkflowRun struct {
	Name       string
	HeadSHA    string
	WorkflowID int64
	Status     string
	// Conclusion is "" while the workflow run runs.
	Conclusion string
}

type workflowRun struct {
	repository string
	WorkflowRun
}

type pullRequest struct {
	repository string
	PullRequest
}

type checkRun struct {
	repository string
	CheckRun
}

type annotationJSON struct {
	Path      string `json:"path"`
	StartLine int64  `json:"start_line"`
	Message   string `json:"message"`
}

// OpenPullRequest opens a pull request from head into main as the App bot, and gives its number.
func (g *FakeGitHub) OpenPullRequest(repository, title, head string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.insertPullRequest(repository, PullRequest{Title: title, Head: head, Base: "main"})
}

// PullRequests gives the pull requests that the fake GitHub opened in repository, in the order of their opening.
func (g *FakeGitHub) PullRequests(repository string) []PullRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	var found []PullRequest
	for _, pull := range g.pullRequests {
		if pull.repository == repository {
			found = append(found, pull.PullRequest)
		}
	}
	return found
}

// SetDraft sets the draft status of the pull request number of repository.
func (g *FakeGitHub) SetDraft(repository string, number int64, draft bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	index := slices.IndexFunc(g.pullRequests, func(pull pullRequest) bool { return pull.repository == repository && pull.Number == number })
	g.pullRequests[index].Draft = draft
}

// MergePullRequest marks the pull request as merged and closed. The branch stays.
func (g *FakeGitHub) MergePullRequest(repository string, number int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.tick()
	found := g.issues[issueKey{repository, number}]
	found.state = "closed"
	found.merged = true
	found.updatedAt = now
}

// RefuseMerge makes the pull request refuse a merge with reason, for example for a rule of the base branch. An empty
// reason lifts the refusal.
func (g *FakeGitHub) RefuseMerge(repository string, number int64, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mergeRefusals[issueKey{repository, number}] = reason
}

// MergeCalls gives the number of requests to merge a pull request.
func (g *FakeGitHub) MergeCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mergeCalls
}

// CheckRunReads gives the number of requests for the check runs of a commit.
func (g *FakeGitHub) CheckRunReads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.checkRunReads
}

// AfterNextReviewedCheckRunRead makes the fake run f once, at the end of the first request for the check runs of a
// commit after a GraphQL answer gave a review of a pull request. The function must not call a method that takes the
// lock of the fake.
func (g *FakeGitHub) AfterNextReviewedCheckRunRead(f func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.afterCheckRunRead = f
}

// SetCreatedAt sets the creation time of the issue or the pull request to seconds after the Unix epoch.
func (g *FakeGitHub) SetCreatedAt(repository string, number, seconds int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.createdAt[issueKey{repository, number}] = seconds
}

// SetBehind makes the pull request give the mergeable_state behind while its base is not an ancestor of its head.
func (g *FakeGitHub) SetBehind(repository string, number int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.behind[issueKey{repository, number}] = true
}

// SetMergeableUnknown makes the pull request give mergeable null, as GitHub does while it calculates the merge, or
// gives its mergeable back.
func (g *FakeGitHub) SetMergeableUnknown(repository string, number int64, unknown bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unknownMergeable[issueKey{repository, number}] = unknown
}

// CheckRuns gives the check runs of repository, in the order of their creation.
func (g *FakeGitHub) CheckRuns(repository string) []CheckRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	var found []CheckRun
	for _, run := range g.checkRuns {
		if run.repository == repository {
			found = append(found, run.CheckRun)
		}
	}
	return found
}

// AddCheckRun adds the check run of another App to repository, and gives its id.
func (g *FakeGitHub) AddCheckRun(repository string, run CheckRun) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkRuns = append(g.checkRuns, checkRun{repository, run})
	return int64(len(g.checkRuns))
}

// SetCheckRunStatus sets the status and the conclusion of the check run id.
func (g *FakeGitHub) SetCheckRunStatus(id int64, status, conclusion string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkRuns[id-1].Status = status
	g.checkRuns[id-1].Conclusion = conclusion
}

// AddWorkflowRun adds the workflow run of GitHub Actions to repository, and gives its id.
func (g *FakeGitHub) AddWorkflowRun(repository string, run WorkflowRun) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.workflowRuns = append(g.workflowRuns, workflowRun{repository, run})
	return int64(len(g.workflowRuns))
}

// SetWorkflowRunStatus sets the status and the conclusion of the workflow run id.
func (g *FakeGitHub) SetWorkflowRunStatus(id int64, status, conclusion string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.workflowRuns[id-1].Status = status
	g.workflowRuns[id-1].Conclusion = conclusion
}

// AddAnnotation adds an annotation on line of path with message to the check run id.
func (g *FakeGitHub) AddAnnotation(id int64, path string, line int64, message string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.annotations[id] = append(g.annotations[id], annotationJSON{path, line, message})
}

// SetCheckRunApp makes the App with slug the App of the check run id. A check run has no App before.
func (g *FakeGitHub) SetCheckRunApp(id int64, slug string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkRunApps[id] = slug
}

// AddJobLog sets the job log of the check run id. A check run with no job log gives 404.
func (g *FakeGitHub) AddJobLog(id int64, log string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.jobLogs[id] = log
}

// insertPullRequest adds the pull request with the next free number of repository, as the App bot. The caller must
// hold the lock.
func (g *FakeGitHub) insertPullRequest(repository string, pull PullRequest) int64 {
	key := issueKey{repository, 1}
	for other := range g.issues {
		if other.repository == repository {
			key.number = max(key.number, other.number+1)
		}
	}
	g.insertIssue(key, pull.Title, pull.Body, botLogin(g.appOf(repository)))
	g.issues[key].pullRequest = true
	pull.Number = key.number
	g.pullRequests = append(g.pullRequests, pullRequest{repository, pull})
	return key.number
}

func (g *FakeGitHub) createPullRequest(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}
	if !decode(w, r, &request) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	number := g.insertPullRequest(repository(r), PullRequest{Title: request.Title, Body: request.Body, Head: request.Head, Base: request.Base, Draft: request.Draft})
	body := g.pullRequestJSON(repository(r), number)
	// GitHub computes mergeable after the creation.
	body["mergeable"] = nil
	writeJSON(w, http.StatusCreated, body)
}

// pullRequestJSON gives the pull request as GitHub does. It is not mergeable when git merge-tree of the base and the
// head in the remote finds a conflict. The caller must hold the lock.
func (g *FakeGitHub) pullRequestJSON(repository string, number int64) map[string]any {
	key := issueKey{repository, number}
	index := slices.IndexFunc(g.pullRequests, func(pull pullRequest) bool { return pull.repository == repository && pull.Number == number })
	pull := g.pullRequests[index]
	remote := g.Remote(repository)
	mergeable := gitCommand(remote, "merge-tree", "--write-tree", pull.Base, pull.Head).Run() == nil
	state := "clean"
	if g.behind[key] && gitCommand(remote, "merge-base", "--is-ancestor", pull.Base, pull.Head).Run() != nil {
		state = "behind"
	}
	found := g.issues[key]
	var mergeableValue any = mergeable
	if g.unknownMergeable[key] {
		mergeableValue = nil
	}
	return map[string]any{
		"number":          number,
		"node_id":         fmt.Sprintf("PR_%d", number),
		"html_url":        fmt.Sprintf("https://github.com/%s/pull/%d", repository, number),
		"state":           found.state,
		"merged":          found.merged,
		"head":            map[string]string{"sha": g.headOf(repository, number), "ref": pull.Head},
		"draft":           pull.Draft,
		"mergeable":       mergeableValue,
		"mergeable_state": state,
		"created_at":      timestamp(g.createdAt[key]),
		"labels":          g.issueJSON(key).Labels,
	}
}

// headOf gives the id of the head commit of the pull request, or "" for a pull request with no branch. The caller must
// hold the lock.
func (g *FakeGitHub) headOf(repository string, number int64) string {
	index := slices.IndexFunc(g.pullRequests, func(pull pullRequest) bool { return pull.repository == repository && pull.Number == number })
	if index < 0 {
		return ""
	}
	head, err := gitCommand(g.Remote(repository), "rev-parse", g.pullRequests[index].Head).Output()
	if err != nil {
		g.t.Errorf("rev-parse %s: %v", g.pullRequests[index].Head, err)
	}
	return strings.TrimSpace(string(head))
}

func (g *FakeGitHub) getPullRequest(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.ParseInt(r.PathValue("number"), 10, 64)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.ContainsFunc(g.pullRequests, func(pull pullRequest) bool { return pull.repository == repository(r) && pull.Number == number }) {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, g.pullRequestJSON(repository(r), number))
}

// mergePullRequest squash merges the pull request into its base branch: it adds one commit with the merge tree of the
// base and the head, and sets the pull request to merged. It refuses with status 409 a sha that is not the head, and
// with status 405 a pull request that is closed, a draft, in conflict, or set to refuse by RefuseMerge.
func (g *FakeGitHub) mergePullRequest(w http.ResponseWriter, r *http.Request) {
	var request struct {
		SHA           string `json:"sha"`
		MergeMethod   string `json:"merge_method"`
		CommitTitle   string `json:"commit_title"`
		CommitMessage string `json:"commit_message"`
	}
	if !decode(w, r, &request) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mergeCalls++
	key, ok := g.issue(w, r)
	if !ok {
		return
	}
	pull := g.pullRequests[slices.IndexFunc(g.pullRequests, func(pull pullRequest) bool { return pull.repository == key.repository && pull.Number == key.number })]
	found := g.issues[key]
	remote := g.Remote(key.repository)
	tree, treeErr := gitCommand(remote, "merge-tree", "--write-tree", pull.Base, pull.Head).Output()
	switch {
	case request.SHA != g.headOf(key.repository, key.number):
		message(w, http.StatusConflict, "Head branch was modified. Review and try the merge again.")
		return
	case g.mergeRefusals[key] != "":
		message(w, http.StatusMethodNotAllowed, g.mergeRefusals[key])
		return
	case found.state != "open" || pull.Draft || treeErr != nil:
		message(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
		return
	}
	if request.MergeMethod != "squash" {
		message(w, http.StatusBadRequest, "The fake GitHub merges with merge_method squash only")
		return
	}
	squash, err := gitCommand(remote, "commit-tree", strings.Fields(string(tree))[0], "-p", pull.Base, "-m", pull.Title).Output()
	if err != nil {
		g.t.Errorf("commit-tree: %v", err)
	}
	sha := strings.TrimSpace(string(squash))
	g.git(remote, "update-ref", "refs/heads/"+pull.Base, sha)
	found.state = "closed"
	found.merged = true
	found.updatedAt = g.tick()
	writeJSON(w, http.StatusOK, map[string]any{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
}

// markReadyForReview answers the GraphQL mutation markPullRequestReadyForReview. The caller must hold the lock.
func (g *FakeGitHub) markReadyForReview(w http.ResponseWriter, id string) {
	index := slices.IndexFunc(g.pullRequests, func(pull pullRequest) bool { return fmt.Sprintf("PR_%d", pull.Number) == id })
	if index < 0 {
		writeJSON(w, http.StatusOK, map[string]any{"data": nil, "errors": []map[string]string{{"message": "Could not resolve to a node"}}})
		return
	}
	g.pullRequests[index].Draft = false
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"markPullRequestReadyForReview": map[string]any{"clientMutationId": nil}}})
}

// convertToDraft answers the GraphQL mutation convertPullRequestToDraft. The caller must hold the lock.
func (g *FakeGitHub) convertToDraft(w http.ResponseWriter, id string) {
	index := slices.IndexFunc(g.pullRequests, func(pull pullRequest) bool { return fmt.Sprintf("PR_%d", pull.Number) == id })
	if index < 0 {
		writeJSON(w, http.StatusOK, map[string]any{"data": nil, "errors": []map[string]string{{"message": "Could not resolve to a node"}}})
		return
	}
	g.pullRequests[index].Draft = true
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"convertPullRequestToDraft": map[string]any{"clientMutationId": nil}}})
}

func (g *FakeGitHub) createCheckRun(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string          `json:"name"`
		HeadSHA    string          `json:"head_sha"`
		Status     string          `json:"status"`
		Conclusion string          `json:"conclusion"`
		Output     *CheckRunOutput `json:"output"`
	}
	if !decode(w, r, &request) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkRuns = append(g.checkRuns, checkRun{repository(r), CheckRun{request.Name, request.HeadSHA, request.Status, request.Conclusion, request.Output}})
	writeJSON(w, http.StatusCreated, map[string]any{"id": len(g.checkRuns)})
}

// checkRunOf gives the index of the check run of the id in the path of r, when the check run is in the repository of
// the path. The caller must hold the lock.
func (g *FakeGitHub) checkRunOf(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 || id > len(g.checkRuns) || g.checkRuns[id-1].repository != repository(r) {
		notFound(w)
		return 0, false
	}
	return id - 1, true
}

func (g *FakeGitHub) updateCheckRun(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name       string          `json:"name"`
		Status     string          `json:"status"`
		Conclusion string          `json:"conclusion"`
		Output     *CheckRunOutput `json:"output"`
	}
	if !decode(w, r, &request) {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	index, ok := g.checkRunOf(w, r)
	if !ok {
		return
	}
	g.checkRuns[index].Status = request.Status
	g.checkRuns[index].Conclusion = request.Conclusion
	if request.Output != nil {
		g.checkRuns[index].Output = request.Output
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": index + 1})
}

func (g *FakeGitHub) commitCheckRuns(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checkRunReads++
	defer func() {
		if g.afterCheckRunRead != nil && g.reviewRead {
			g.afterCheckRunRead()
			g.afterCheckRunRead = nil
		}
	}()
	runs := []map[string]any{}
	for index, run := range g.checkRuns {
		if run.repository != repository(r) || run.HeadSHA != r.PathValue("sha") {
			continue
		}
		id := int64(index + 1)
		output := map[string]any{"title": nil, "summary": nil}
		if run.Output != nil {
			output = map[string]any{"title": run.Output.Title, "summary": run.Output.Summary}
		}
		body := map[string]any{
			"id":       id,
			"name":     run.Name,
			"status":   run.Status,
			"html_url": fmt.Sprintf("https://github.com/%s/runs/%d", run.repository, id),
			"output":   output,
		}
		if run.Conclusion != "" {
			body["conclusion"] = run.Conclusion
		}
		if slug, ok := g.checkRunApps[id]; ok {
			body["app"] = map[string]string{"slug": slug}
		}
		runs = append(runs, body)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": page(w, r, runs)})
}

func (g *FakeGitHub) checkRunAnnotations(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if index, ok := g.checkRunOf(w, r); ok {
		writeJSON(w, http.StatusOK, page(w, r, append([]annotationJSON{}, g.annotations[int64(index+1)]...)))
	}
}

// jobLogLink sends the client to the job log, as GitHub sends it to a link with no token.
func (g *FakeGitHub) jobLogLink(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, ok := g.jobLogs[id]; !ok {
		notFound(w)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("%s/job-logs/%d", g.URL, id))
	w.WriteHeader(http.StatusFound)
}

func (g *FakeGitHub) jobLog(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	log, ok := g.jobLogs[id]
	if !ok {
		notFound(w)
		return
	}
	_, _ = w.Write([]byte(log))
}

func (g *FakeGitHub) listWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	runs := []map[string]any{}
	for index, run := range g.workflowRuns {
		if run.repository != repository(r) || run.HeadSHA != r.URL.Query().Get("head_sha") {
			continue
		}
		body := map[string]any{"id": index + 1, "name": run.Name, "workflow_id": run.WorkflowID, "head_sha": run.HeadSHA, "status": run.Status}
		if run.Conclusion != "" {
			body["conclusion"] = run.Conclusion
		}
		runs = append(runs, body)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "workflow_runs": page(w, r, runs)})
}
