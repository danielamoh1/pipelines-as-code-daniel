package bitbucketcloud

import (
	"context"
	"fmt"
	"strings"

	"github.com/ktrysmt/go-bitbucket"
	"github.com/mitchellh/mapstructure"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/params/info"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/webvcs"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/webvcs/bitbucketcloud/types"
)

type VCS struct {
	Client        *bitbucket.Client
	Token, APIURL *string
	Username      *string
}

const taskStatusTemplate = `| **Status** | **Duration** | **Name** |
| --- | --- | --- |
{{range $taskrun := .TaskRunList }}|{{ formatCondition $taskrun.Status.Conditions }}|{{ formatDuration $taskrun.Status.StartTime $taskrun.Status.CompletionTime }}|{{ $taskrun.ConsoleLogURL }}|
{{ end }}`

func (v *VCS) GetConfig() *info.VCSConfig {
	return &info.VCSConfig{
		TaskStatusTMPL: taskStatusTemplate,
		APIURL:         bitbucket.DEFAULT_BITBUCKET_API_BASE_URL,
	}
}

func (v *VCS) CreateStatus(_ context.Context, event *info.Event, pacopts *info.PacOpts,
	statusopts webvcs.StatusOpts) error {
	switch statusopts.Conclusion {
	case "skipped":
		statusopts.Conclusion = "STOPPED"
		statusopts.Title = "➖ Skipping this commit"
	case "neutral":
		statusopts.Conclusion = "STOPPED"
		statusopts.Title = "➖ CI has stopped"
	case "failure":
		statusopts.Conclusion = "FAILED"
		statusopts.Title = "❌ Failed"
	case "pending":
		statusopts.Conclusion = "INPROGRESS"
		statusopts.Title = "⚡ CI has started"
	case "success":
		statusopts.Conclusion = "SUCCESSFUL"
		statusopts.Title = "✅ Commit has been validated"
	case "completed":
		statusopts.Conclusion = "SUCCESSFUL"
		statusopts.Title = "✅ Completed"
	}
	detailsURL := pacopts.VCSAPIURL
	if statusopts.DetailsURL != "" {
		detailsURL = statusopts.DetailsURL
	}

	cso := &bitbucket.CommitStatusOptions{
		Key:         pacopts.ApplicationName,
		Url:         detailsURL,
		State:       statusopts.Conclusion,
		Description: statusopts.Title,
	}
	cmo := &bitbucket.CommitsOptions{
		Owner:    event.Owner,
		RepoSlug: event.Repository,
		Revision: event.SHA,
	}

	if v.Client == nil {
		return fmt.Errorf("no token has been set, cannot set status")
	}

	_, err := v.Client.Repositories.Commits.CreateCommitStatus(cmo, cso)
	if err != nil {
		return err
	}
	if statusopts.Conclusion != "STOPPED" && statusopts.Status == "completed" &&
		statusopts.Text != "" && event.EventType == "pull_request" {
		prNumber, err := v.getPullRequestNumber(event.Event)
		if err != nil {
			return err
		}

		_, err = v.Client.Repositories.PullRequests.AddComment(
			&bitbucket.PullRequestCommentOptions{
				Owner:         event.Owner,
				RepoSlug:      event.Repository,
				PullRequestID: prNumber,
				Content: fmt.Sprintf("**%s** - %s\n\n%s", pacopts.ApplicationName,
					statusopts.Title, statusopts.Text),
			})
		if err != nil {
			return err
		}
	}
	return nil
}

func (v *VCS) GetTektonDir(_ context.Context, event *info.Event, path string) (string, error) {
	repoFileOpts := &bitbucket.RepositoryFilesOptions{
		Owner:    event.Owner,
		RepoSlug: event.Repository,
		Ref:      event.SHA,
		Path:     path,
	}

	repositoryFiles, err := v.Client.Repositories.Repository.ListFiles(repoFileOpts)
	if err != nil {
		return "", err
	}

	return v.concatAllYamlFiles(repositoryFiles, event)
}

func (v *VCS) GetFileInsideRepo(_ context.Context, runevent *info.Event, path string, targetBranch string) (string,
	error) {
	branch := runevent.HeadBranch
	if targetBranch != "" {
		branch = targetBranch
	}
	return v.getBlob(runevent, branch, path)
}

func (v *VCS) SetClient(_ context.Context, opts *info.PacOpts) error {
	if opts.VCSUser == "" {
		return fmt.Errorf("no webvcs_api_user has been set in the repo crd")
	}
	if opts.VCSToken == "" {
		return fmt.Errorf("no webvcs_api_api_secret has been set in the repo crd")
	}
	v.Client = bitbucket.NewBasicAuth(opts.VCSUser, opts.VCSToken)
	v.Token = &opts.VCSToken
	v.Username = &opts.VCSUser
	return nil
}

func (v *VCS) GetCommitInfo(_ context.Context, event *info.Event) error {
	response, err := v.Client.Repositories.Commits.GetCommits(&bitbucket.CommitsOptions{
		Owner:       event.Owner,
		RepoSlug:    event.Repository,
		Branchortag: event.SHA,
	})
	if err != nil {
		return err
	}
	commitMap, ok := response.(map[string]interface{})
	if !ok {
		return fmt.Errorf("cannot convert")
	}
	values, ok := commitMap["values"].([]interface{})
	if !ok {
		return fmt.Errorf("cannot convert")
	}
	if len(values) == 0 {
		return fmt.Errorf("we did not get commit information from commit: %s", event.SHA)
	}
	commitinfo := &types.Commit{}
	err = mapstructure.Decode(values[0], commitinfo)
	if err != nil {
		return err
	}

	// Some silliness since we get first the account id and we fill it properly after
	event.SHATitle = commitinfo.Message
	event.SHAURL = commitinfo.Links.HTML.HRef
	event.SHA = commitinfo.Hash

	// now to get the default branch from repository.Get
	repo, err := v.Client.Repositories.Repository.Get(&bitbucket.RepositoryOptions{
		Owner:    event.Owner,
		RepoSlug: event.Repository,
	})
	if err != nil {
		return err
	}
	event.DefaultBranch = repo.Mainbranch.Name
	return nil
}

func (v *VCS) concatAllYamlFiles(objects []bitbucket.RepositoryFile, runevent *info.Event) (string, error) {
	var allTemplates string

	for _, value := range objects {
		if strings.HasSuffix(value.Path, ".yaml") ||
			strings.HasSuffix(value.Path, ".yml") {
			data, err := v.getBlob(runevent, runevent.HeadBranch, value.Path)
			if err != nil {
				return "", err
			}

			if allTemplates != "" && !strings.HasPrefix(data, "---") {
				allTemplates += "---"
			}
			allTemplates += "\n" + data + "\n"
		}
	}
	return allTemplates, nil
}

func (v *VCS) getBlob(runevent *info.Event, ref, path string) (string, error) {
	blob, err := v.Client.Repositories.Repository.GetFileBlob(&bitbucket.RepositoryBlobOptions{
		Owner:    runevent.Owner,
		RepoSlug: runevent.Repository,
		Ref:      ref,
		Path:     path,
	})
	if err != nil {
		return "", fmt.Errorf("cannot find %s on branch %s in repo %s/%s", path, ref, runevent.Owner, runevent.Repository)
	}
	return blob.String(), nil
}

func (v *VCS) getPullRequestNumber(eventPayload interface{}) (string, error) {
	prevent, ok := eventPayload.(*types.PullRequestEvent)
	if !ok {
		return "", fmt.Errorf("cannot convert event to PullRequestEvent")
	}
	prID := prevent.PullRequest.ID
	if prID == 0 {
		return "", fmt.Errorf("could not detect pull request ID")
	}
	return fmt.Sprintf("%d", prID), nil
}
