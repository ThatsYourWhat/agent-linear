package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const issueFields = `id identifier title description url updatedAt createdAt
 project { id name } team { id } state { id name type } assignee { id isMe }
 relations { nodes { type relatedIssue { identifier state { type } } } }
 inverseRelations { nodes { type issue { id identifier state { type } } } }`
const queueQuery = `query Queue($filter: IssueFilter!, $after: String) {
 issues(first: 50, after: $after, filter: $filter, orderBy: createdAt) {
 nodes { ` + issueFields + ` } pageInfo { hasNextPage endCursor }
 } }`

func (r *runner) graphql(query string, variables map[string]any, target any) error {
	key := os.Getenv("LINEAR_API_KEY")
	if key == "" {
		return errors.New("set LINEAR_API_KEY in the environment")
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(req)
	if err != nil {
		return errors.New("Linear request failed; verify connectivity and credentials")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Linear HTTP %d; verify credentials, scope and query", response.StatusCode)
	}
	var payload struct {
		Data   json.RawMessage `json:"data"`
		Errors json.RawMessage `json:"errors"`
	}
	if err = json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return errors.New("invalid Linear JSON response")
	}
	if (len(payload.Errors) > 0 && string(payload.Errors) != "null" && string(payload.Errors) != "[]") || len(payload.Data) == 0 || string(payload.Data) == "null" {
		return errors.New("Linear GraphQL failed; verify credentials, scope and query")
	}
	if err = json.Unmarshal(payload.Data, target); err != nil {
		return errors.New("invalid Linear data response")
	}
	return nil
}

func (r *runner) issues(project string, rt *route, issueID string) ([]map[string]any, error) {
	eq := func(s string) any { return map[string]any{"id": map[string]any{"eq": s}} }
	filter := map[string]any{"project": eq(project), "team": eq(rt.TeamID), "state": eq(rt.ReadyStateID)}
	filter["or"] = []any{map[string]any{"assignee": map[string]any{"null": true}}, map[string]any{"assignee": map[string]any{"isMe": map[string]any{"eq": true}}}}
	if rt.Label != "" {
		filter["labels"] = map[string]any{"name": map[string]any{"eq": rt.Label}}
	}
	if issueID != "" {
		filter["id"] = map[string]any{"eq": issueID}
	}
	var after any
	var all []map[string]any
	for {
		var data struct {
			Issues *struct {
				Nodes    []map[string]any `json:"nodes"`
				PageInfo struct {
					HasNext bool   `json:"hasNextPage"`
					Cursor  string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := r.graphql(queueQuery, map[string]any{"filter": filter, "after": after}, &data); err != nil {
			return nil, err
		}
		if data.Issues == nil {
			return nil, errors.New("Linear returned no issues page")
		}
		all = append(all, data.Issues.Nodes...)
		if !data.Issues.PageInfo.HasNext {
			return all, nil
		}
		cursor := data.Issues.PageInfo.Cursor
		if cursor == "" || cursor == after {
			return nil, errors.New("Linear returned a non-advancing pagination cursor")
		}
		after = cursor
	}
}
func (r *runner) issue(id string) (map[string]any, error) {
	var data struct {
		Issue map[string]any `json:"issue"`
	}
	if err := r.graphql(`query RunIssue($id: String!) { issue(id: $id) { `+issueFields+` } }`, map[string]any{"id": id}, &data); err != nil {
		return nil, err
	}
	if textAt(data.Issue, "id") != id && textAt(data.Issue, "identifier") != id {
		return nil, errors.New("Linear returned no matching current issue")
	}
	return data.Issue, nil
}
func (r *runner) claimableIssue(id, project string, rt *route) (map[string]any, error) {
	current, err := r.issue(id)
	if err != nil {
		return nil, err
	}
	if textAt(current, "state", "id") != rt.ReadyStateID || textAt(current, "assignee", "id") != "" && !boolAt(current["assignee"], "isMe") {
		return nil, nil
	}
	if err = validateIssue(current, project, rt); err != nil {
		return nil, err
	}
	return current, nil
}

// openBlockers returns the identifiers of issues that still block issue: open
// blockers plus terminal blockers whose PR is not yet merged into the base
// branch. A terminal blocker with no recorded PR is treated as resolved.
func (r *runner) openBlockers(db *sql.DB, issue map[string]any) ([]string, error) {
	blockers := blockedBy(issue)
	relations, _ := issue["inverseRelations"].(map[string]any)
	nodes, _ := relations["nodes"].([]any)
	for _, node := range nodes {
		relation, ok := node.(map[string]any)
		if !ok || textAt(relation, "type") != "blocks" {
			continue
		}
		blocker := relation["issue"]
		if !terminalStates[textAt(blocker, "state", "type")] {
			continue // already reported by blockedBy
		}
		pr := r.recordedPR(db, textAt(blocker, "id"))
		if pr == "" {
			continue // terminal and no known PR: resolved
		}
		merged, err := r.prMerged(pr)
		if err != nil {
			return nil, err
		}
		if !merged {
			if identifier := textAt(blocker, "identifier"); identifier != "" {
				blockers = append(blockers, identifier)
			}
		}
	}
	return blockers, nil
}

// recordedPR returns the pull request URL recorded for a completed issue, if any.
func (r *runner) recordedPR(db *sql.DB, issueID string) string {
	if issueID == "" {
		return ""
	}
	var pr string
	if err := db.QueryRow("SELECT pr_url FROM runs WHERE issue_id=?", issueID).Scan(&pr); err != nil {
		return ""
	}
	return pr
}

// prMerged reports whether the pull request has been merged.
func (r *runner) prMerged(prURL string) (bool, error) {
	out, err := r.command("", "gh", "pr", "view", prURL, "--json", "mergedAt", "--jq", ".mergedAt != null")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}
func (r *runner) workflowStates(team string) (map[string]string, error) {
	var data struct {
		States *struct {
			Nodes    []struct{ ID, Name string } `json:"nodes"`
			PageInfo struct {
				HasNext bool `json:"hasNextPage"`
			} `json:"pageInfo"`
		} `json:"workflowStates"`
	}
	err := r.graphql(`query RunStates($team: ID!, $names: [String!]!) {
 workflowStates(first: 3, filter: {team: {id: {eq: $team}}, name: {in: $names}}) {
 nodes { id name } pageInfo { hasNextPage } } }`, map[string]any{"team": team, "names": []string{"In Progress", "In Review"}}, &data)
	if err != nil {
		return nil, err
	}
	if data.States == nil {
		return nil, errors.New("Linear returned no workflow states")
	}
	states := map[string]string{}
	for _, name := range []string{"In Progress", "In Review"} {
		count := 0
		for _, state := range data.States.Nodes {
			if state.Name == name {
				count++
				states[name] = state.ID
			}
		}
		if count != 1 || data.States.PageInfo.HasNext || states[name] == "" {
			return nil, fmt.Errorf("Linear team requires exactly one %s status", name)
		}
	}
	return states, nil
}
func (r *runner) viewer() (string, string, error) {
	var data struct {
		Viewer struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"viewer"`
	}
	if err := r.graphql(`query RunUser { viewer { id name } }`, map[string]any{}, &data); err != nil {
		return "", "", err
	}
	if !uuidRE.MatchString(data.Viewer.ID) {
		return "", "", errors.New("Linear returned no valid current user")
	}
	return data.Viewer.ID, data.Viewer.Name, nil
}
func (r *runner) updateIssueState(issueID, stateID, folder, stage, assigneeID string, assign bool) error {
	input := map[string]any{"stateId": stateID}
	if assign {
		input["assigneeId"] = assigneeID
	}
	variables := map[string]any{"id": issueID, "input": input}
	if err := writeJSON(filepath.Join(folder, stage+"-state-request.json"), variables); err != nil {
		return err
	}
	var data struct {
		Update map[string]any `json:"issueUpdate"`
	}
	err := r.graphql(`mutation RunState($id: String!, $input: IssueUpdateInput!) {
 issueUpdate(id: $id, input: $input) { success issue { id state { id } assignee { id } } } }`, variables, &data)
	if err != nil {
		return err
	}
	if !boolAt(data.Update, "success") || textAt(data.Update, "issue", "id") != issueID || textAt(data.Update, "issue", "state", "id") != stateID || assigneeID != "" && textAt(data.Update, "issue", "assignee", "id") != assigneeID {
		return fmt.Errorf("Linear %s status update was not confirmed (including requested assignee)", stage)
	}
	return writeJSON(filepath.Join(folder, stage+"-state.json"), data.Update)
}
func (r *runner) postComment(issueID, body, folder, stage string) error {
	err := writeFile(filepath.Join(folder, stage+"-comment.txt"), []byte(body+"\n"))
	if err == nil {
		var data struct {
			Comment map[string]any `json:"commentCreate"`
		}
		err = r.graphql(`mutation RunComment($input: CommentCreateInput!) { commentCreate(input: $input) { success comment { id } } }`, map[string]any{"input": map[string]any{"issueId": issueID, "body": body}}, &data)
		if err == nil {
			if !boolAt(data.Comment, "success") || textAt(data.Comment, "comment", "id") == "" {
				err = errors.New("Linear comment was not confirmed")
			} else {
				err = writeJSON(filepath.Join(folder, stage+"-comment.json"), data.Comment)
			}
		}
	}
	if err != nil {
		// Notification delivery must not overwrite a saved run outcome or leak provider bodies.
		if e := writeFile(filepath.Join(folder, stage+"-comment-error.txt"), []byte("comment delivery failed\n")); e != nil {
			fmt.Fprintf(r.errOut, "Cannot save %s comment error evidence\n", stage)
		}
		fmt.Fprintf(r.errOut, "Linear %s comment delivery failed; inspect %s\n", stage, folder)
	}
	return err
}

// Read complete event lines without Scanner's default 64 KiB token limit.
func eachLine(reader io.Reader, visit func([]byte) error) error {
	decoder := bufio.NewReader(reader)
	for {
		line, err := decoder.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if e := visit(bytes.TrimSpace(line)); e != nil {
				return e
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
