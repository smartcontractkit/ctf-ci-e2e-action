package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/go-github/v47/github"
	"github.com/kelseyhightower/envconfig"
	"github.com/sethvargo/go-githubactions"
	"golang.org/x/oauth2"
)

const COMPLETED_STATUS = "completed"

type Inputs struct {
	GithubToken   string        `envconfig:"token" required:"true"`
	Owner         string        `envconfig:"owner" default:"smartcontractkit"`
	Repository    string        `envconfig:"repository" required:"true"`
	Branch        string        `envconfig:"branch" required:"true"`
	WorkflowFile  string        `envconfig:"workflow_file" required:"true"`
	Timeout       time.Duration `envconfig:"timeout" default:"5m"`
	RetryInterval time.Duration `envconfig:"retry_interval" default:"15s"`
}

func getInputs() *Inputs {
	var inputs Inputs
	if err := envconfig.Process("", &inputs); err != nil {
		githubactions.Fatalf("could not load inputs from environment: %v", err)
	}

	return &inputs
}

type Outputs struct {
	Status     string
	Conclusion string
}

func (o *Outputs) setOutputs() {
	ao := *o
	val := reflect.ValueOf(ao)
	typeOfS := val.Type()

	for i := 0; i < val.NumField(); i++ {
		k := strings.ToLower(typeOfS.Field(i).Name)
		v := fmt.Sprintf("%v", val.Field(i).Interface())
		githubactions.Infof("Setting output: %s = %s", k, v)
		githubactions.SetOutput(k, v)
	}
}

type Common struct {
	ctx        context.Context
	client     *github.Client
	workflowID int64
}

func (c *Common) connectToGithub(inputs *Inputs) {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: inputs.GithubToken},
	)
	tc := oauth2.NewClient(c.ctx, ts)
	c.client = github.NewClient(tc)
}

func (c *Common) getMostRecentWorkflowRunId(inputs *Inputs) {
	opts := &github.ListWorkflowRunsOptions{
		Branch: inputs.Branch,
	}
	maxRetrys := 5
	var err error
	var workflow *github.WorkflowRun
	var runs *github.WorkflowRuns
	var resp *github.Response
	for i := 0; i < maxRetrys; i++ {
		// if err is nil then it is the first time and we do not need to sleep, otherwise wiat
		if err != nil {
			time.Sleep(inputs.RetryInterval)
		}

		runs, resp, err = c.client.Actions.ListWorkflowRunsByFileName(c.ctx, inputs.Owner, inputs.Repository, inputs.WorkflowFile, opts)
		if err != nil {
			githubactions.Infof("failed to get the workflow: %v", err)
			continue
		}
		if resp.StatusCode != 200 {
			err = fmt.Errorf("failed to get the workflow, status code %d", resp.StatusCode)
			githubactions.Infof("%v", err)
			continue
		}
		if runs.GetTotalCount() < 1 {
			err = fmt.Errorf("failed fo find any workflow runs")
			githubactions.Infof("%v", err)
			continue
		}

		// get the workflow run id from the latest workflow
		var currentWorkflow *github.WorkflowRun
		var timestamp *github.Timestamp
		for i, wf := range runs.WorkflowRuns {
			githubactions.Infof("workflow at index %d had completion status of %s at %s with id %d", i, *wf.Status, wf.CreatedAt.Time.String(), *wf.ID)
			if timestamp == nil {
				timestamp = wf.CreatedAt
			}
			// we only care about uncompleted workflows
			if *wf.Status != COMPLETED_STATUS {
				if currentWorkflow == nil {
					currentWorkflow = wf
				}
				githubactions.Infof("found an active workflow")
				// we want the latest workflow so if this one is newer, use it
				if wf.CreatedAt.After(timestamp.Time) {
					githubactions.Infof("workflow is newer, using it unless we find a newer one")
					currentWorkflow = wf
				}
			}
		}
		if currentWorkflow == nil {
			err = fmt.Errorf("failed to find an unfinished workflow")
			githubactions.Infof("%v", err)
			continue
		}
		workflow = currentWorkflow
		break
	}
	// fail if we ended the retry loop in error
	if err != nil {
		githubactions.Fatalf("did not find any active workflows: %v", err)
	}

	c.workflowID = *workflow.ID
}

func (c *Common) getWorkflowRun(inputs *Inputs) *github.WorkflowRun {
	wfr, resp, err := c.client.Actions.GetWorkflowRunByID(c.ctx, inputs.Owner, inputs.Repository, c.workflowID)
	if err != nil {
		githubactions.Infof("Failed to get the workflow run: %v", err)
		return nil
	}
	if resp.StatusCode != 200 {
		githubactions.Infof("Failed to get the workflow, found status code: %d", resp.StatusCode)
		return nil
	}
	githubactions.Infof("current workflow run status: %v", *wfr.Status)
	return wfr
}

func (c *Common) pollWorkflow(inputs *Inputs) *Outputs {
	stop := false
	var status string
	var latestWorkflow *github.WorkflowRun
	testContext, testCancel := context.WithTimeout(c.ctx, inputs.Timeout)
	defer testCancel()
	ticker := time.NewTicker(inputs.RetryInterval)
	for {
		select {
		case <-ticker.C:
			// check the latest status, if it is completed then we stop
			latestWorkflow = c.getWorkflowRun(inputs)
			status = *latestWorkflow.Status
			stop = status == COMPLETED_STATUS
		case <-testContext.Done():
			// timed out
			stop = true
			ticker.Stop()
			break
		}

		// if stop is true, kill the loop
		if stop {
			break
		}
	}

	if status != COMPLETED_STATUS {
		githubactions.Fatalf("workflow did not reach completed status, id: %d", c.workflowID)
	}

	return &Outputs{
		Status:     status,
		Conclusion: *latestWorkflow.Conclusion,
	}
}

func main() {
	inputs := getInputs()

	common := &Common{
		ctx: context.Background(),
	}

	common.connectToGithub(inputs)
	common.getMostRecentWorkflowRunId(inputs)

	outputs := common.pollWorkflow(inputs)
	outputs.setOutputs()
}
