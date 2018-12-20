/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package trigger

import (
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

const (
	pluginName = "trigger"
)

func init() {
	plugins.RegisterGenericCommentHandler(pluginName, handleGenericCommentEvent, helpProvider)
	plugins.RegisterPullRequestHandler(pluginName, handlePullRequest, helpProvider)
	plugins.RegisterPushEventHandler(pluginName, handlePush, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	configInfo := map[string]string{}
	for _, orgRepo := range enabledRepos {
		parts := strings.Split(orgRepo, "/")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid repo in enabledRepos: %q", orgRepo)
		}
		org, repoName := parts[0], parts[1]
		if trigger := config.TriggerFor(org, repoName); trigger != nil && trigger.TrustedOrg != "" {
			org = trigger.TrustedOrg
		}
		configInfo[orgRepo] = fmt.Sprintf("The trusted Github organization for this repository is %q.", org)
	}
	pluginHelp := &pluginhelp.PluginHelp{
		Description: `The trigger plugin starts tests in reaction to commands and pull request events. It is responsible for ensuring that test jobs are only run on trusted PRs. A PR is considered trusted if the author is a member of the 'trusted organization' for the repository or if such a member has left an '/ok-to-test' command on the PR.
<br>Trigger starts jobs automatically when a new trusted PR is created or when an untrusted PR becomes trusted, but it can also be used to start jobs manually via the '/test' command.
<br>The '/retest' command can be used to rerun jobs that have reported failure.`,
		Config: configInfo,
	}
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/ok-to-test",
		Description: "Marks a PR as 'trusted' and starts tests.",
		Featured:    false,
		WhoCanUse:   "Members of the trusted organization for the repo.",
		Examples:    []string{"/ok-to-test"},
	})
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/test (<job name>|all)",
		Description: "Manually starts a/all test job(s).",
		Featured:    true,
		WhoCanUse:   "Anyone can trigger this command on a trusted PR.",
		Examples:    []string{"/test all", "/test pull-bazel-test"},
	})
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/retest",
		Description: "Rerun test jobs that have failed.",
		Featured:    true,
		WhoCanUse:   "Anyone can trigger this command on a trusted PR.",
		Examples:    []string{"/retest"},
	})
	return pluginHelp, nil
}

type githubClient interface {
	AddLabel(org, repo string, number int, label string) error
	BotName() (string, error)
	IsCollaborator(org, repo, user string) (bool, error)
	IsMember(org, user string) (bool, error)
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	GetRef(org, repo, ref string) (string, error)
	CreateComment(owner, repo string, number int, comment string) error
	ListIssueComments(owner, repo string, issue int) ([]github.IssueComment, error)
	CreateStatus(owner, repo, ref string, status github.Status) error
	GetCombinedStatus(org, repo, ref string) (*github.CombinedStatus, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	RemoveLabel(org, repo string, number int, label string) error
	DeleteStaleComments(org, repo string, number int, comments []github.IssueComment, isStale func(github.IssueComment) bool) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
}

type kubeClient interface {
	CreateProwJob(kube.ProwJob) (kube.ProwJob, error)
}

// Client holds the necessary structures to work with prow via logging, github, kubernetes and its configuration.
//
// TODO(fejta): consider exporting an interface rather than a struct
type Client struct {
	GitHubClient githubClient
	KubeClient   kubeClient
	Config       *config.Config
	Logger       *logrus.Entry
}

type trustedUserClient interface {
	IsCollaborator(org, repo, user string) (bool, error)
	IsMember(org, user string) (bool, error)
}

func getClient(pc plugins.Agent) Client {
	return Client{
		GitHubClient: pc.GitHubClient,
		Config:       pc.Config,
		KubeClient:   pc.KubeClient,
		Logger:       pc.Logger,
	}
}

func handlePullRequest(pc plugins.Agent, pr github.PullRequestEvent) error {
	org, repo, _ := orgRepoAuthor(pr.PullRequest)
	return handlePR(getClient(pc), pc.PluginConfig.TriggerFor(org, repo), pr)
}

func handleGenericCommentEvent(pc plugins.Agent, gc github.GenericCommentEvent) error {
	return handleGenericComment(getClient(pc), pc.PluginConfig.TriggerFor(gc.Repo.Owner.Login, gc.Repo.Name), gc)
}

func handlePush(pc plugins.Agent, pe github.PushEvent) error {
	return handlePE(getClient(pc), pe)
}

// TrustedUser returns true if user is trusted in repo.
//
// Trusted users are either repo collaborators, org members or trusted org members.
// Whether repo collaborators and/or a second org is trusted is configured by trigger.
func TrustedUser(ghc trustedUserClient, trigger *plugins.Trigger, user, org, repo string) (bool, error) {
	// First check if user is a collaborator, assuming this is allowed
	allowCollaborators := trigger == nil || !trigger.OnlyOrgMembers
	if allowCollaborators {
		if ok, err := ghc.IsCollaborator(org, repo, user); err != nil {
			return false, fmt.Errorf("error in IsCollaborator: %v", err)
		} else if ok {
			return true, nil
		}
	}

	// TODO(fejta): consider dropping support for org checks in the future.

	// Next see if the user is an org member
	if member, err := ghc.IsMember(org, user); err != nil {
		return false, fmt.Errorf("error in IsMember(%s): %v", org, err)
	} else if member {
		return true, nil
	}

	// Determine if there is a second org to check
	if trigger == nil || trigger.TrustedOrg == "" || trigger.TrustedOrg == org {
		return false, nil // No trusted org and/or it is the same
	}

	// Check the second trusted org.
	member, err := ghc.IsMember(trigger.TrustedOrg, user)
	if err != nil {
		return false, fmt.Errorf("error in IsMember(%s): %v", trigger.TrustedOrg, err)
	}
	return member, nil
}

// RunOrSkipRequested evaluates requestJobs to determine which config.Presubmits to
// run and which ones to skip and once execute the ones that should be ran.
func RunOrSkipRequested(c Client, pr *github.PullRequest, requestedJobs []config.Presubmit, forceRunContexts sets.String, body, eventGUID string) error {
	org := pr.Base.Repo.Owner.Login
	repo := pr.Base.Repo.Name
	number := pr.Number

	baseSHA, err := c.GitHubClient.GetRef(org, repo, "heads/"+pr.Base.Ref)
	if err != nil {
		return err
	}

	// For each job determine if any sharded version of the job runs.
	// This in turn determines which jobs to run and which contexts to mark as "Skipped".
	//
	// Note: Job sharding is achieved with presubmit configurations that overlap on
	// name, but run under disjoint circumstances. For example, a job 'foo' can be
	// sharded to have different pod specs for different branches by
	// creating 2 presubmit configurations with the name foo, but different pod
	// specs, and specifying different branches for each job.
	var toRunJobs []config.Presubmit
	toRun := sets.NewString()
	toSkip := sets.NewString()
	for _, job := range requestedJobs {
		// jobs that don't run on the branch in question are in the forced list as
		// that is how branch sharding is implemented, so we need to do that check
		// twice
		runs := job.RunsAgainstBranch(pr.Base.Ref) && forceRunContexts.Has(job.Context)
		if !runs {
			runs, err = job.ShouldRun(pr.Base.Ref, body, config.NewGitHubDeferredChangedFilesProvider(c.GitHubClient, org, repo, number))
			if err != nil {
				return err
			}
		}
		if runs {
			toRunJobs = append(toRunJobs, job)
			toRun.Insert(job.Context)
		} else if !job.SkipReport {
			// we need to post context statuses for all jobs
			toSkip.Insert(job.Context)
		}
	}
	// 'Skip' any context that is requested, but doesn't have any job shards that
	// will run.
	for _, context := range toSkip.Difference(toRun).List() {
		if err := c.GitHubClient.CreateStatus(org, repo, pr.Head.SHA, github.Status{
			State:       github.StatusSuccess,
			Context:     context,
			Description: "Skipped",
		}); err != nil {
			return err
		}
	}

	var errors []error
	for _, job := range toRunJobs {
		c.Logger.Infof("Starting %s build.", job.Name)
		pj := pjutil.NewPresubmit(*pr, baseSHA, job, eventGUID)
		c.Logger.WithFields(pjutil.ProwJobFields(&pj)).Info("Creating a new prowjob.")
		if _, err := c.KubeClient.CreateProwJob(pj); err != nil {
			errors = append(errors, err)
		}
	}
	if len(errors) > 0 {
		return fmt.Errorf("errors starting jobs: %v", errors)
	}
	return nil
}
