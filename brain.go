package monty

import (
	"bytes"
	"github.com/amyangfei/redlock-go/redlock"
	"github.com/google/go-github/github"
	"gopkg.in/redis.v3"
	"log"
	"text/template"
)

type Brain struct {
	client *github.Client
	repos  *RepositoryList
	memory *Memory
	lock   *redlock.RedLock
}

func NewBrain(client *github.Client, redisOptions *redis.Options) *Brain {
	ret := new(Brain)
	ret.memory = NewMemory(redisOptions)
	lock, err := redlock.NewRedLock([]string{redisOptions.Addr})
	if err != nil {
		panic(err)
	}
	ret.lock = lock
	ret.client = client
	ret.repos = NewRepolist(client)
	return ret
}

type Feedback struct {
	Count  uint
	People []string
}

func buildComment(review Review) string {

	feedback, err := template.New("feedback").
		Parse("I've reviewed this pull request:\n\n" +
		"{{template \"subconditions\" .Condition.Subconditions}}")

	if err != nil {
		panic(err)
	}

	feedback, err = feedback.Parse(
		"{{define \"subconditions\"}}" +
			"{{range .}} ✔ {{.Name}}: {{.Message}}\n" +
			"{{template \"subconditions\" .Subconditions}}" +
			"{{end}}" +
			"{{end}}")

	if err != nil {
		panic(err)
	}

	var feedbackBuf bytes.Buffer
	err = feedback.Execute(&feedbackBuf, review)

	if err != nil {
		panic(err)
	}

	return feedbackBuf.String()
}

func buildCommitMessage(review Review) string {
	commitTemplate, err := template.New("commit").
		Parse("Automatic merge after review:\n\n" +
		"{{template \"subconditions\" .Condition.Subconditions}}" +
		"{{define \"subconditions\"}}" +
		"{{range .}} ✔ {{.Name}}: {{.Message}}\n" +
		"{{template \"subconditions\" .Subconditions}}" +
		"{{end}}" +
		"{{end}}")

	if err != nil {
		panic(err)
	}

	var commitBuf bytes.Buffer
	err = commitTemplate.Execute(&commitBuf, review)

	if err != nil {
		panic(err)
	}

	return commitBuf.String()
}

func (self *Brain) MergeReview(review Review) {
	if !review.Condition.Passed {
		panic("Review not passed!")
	}

	reviewFeedback := buildComment(review)
	commitMessage := buildCommitMessage(review)

	log.Printf("Commenting with %v", reviewFeedback)
	log.Printf("Merging with %v", commitMessage)

	self.client.Issues.CreateComment(*review.PullRequest.Repository.Owner, *review.PullRequest.Repository.Name, review.PullRequest.Number, &github.IssueComment{
		Body: &reviewFeedback,
	})
	self.client.PullRequests.Merge(*review.PullRequest.Repository.Owner, *review.PullRequest.Repository.Name, review.PullRequest.Number, commitMessage)
}

func (self *Brain) SyncRepositories() {
	self.repos.EnableHooks()

	opts := github.PullRequestListOptions{
		State: "all",
	}

	opts.ListOptions.PerPage = 100

	for _, repo := range *self.repos.List() {
		opts.ListOptions.Page = 1
		for true {
			prs, _, _ := self.client.PullRequests.List(*repo.Owner, *repo.Name, &opts)
			for _, pr := range prs {
				merged := false
				if pr.Merged != nil && *pr.Merged {
					merged = true
				}
				newPR := PullRequest{
					Number:     *pr.Number,
					Body:       *pr.Body,
					User:       *pr.User.Login,
					Title:      *pr.Title,
					SHA:        *pr.Head.SHA,
					Repository: repo,
					Merged:     merged,
				}

				self.memory.RememberPullRequest(&newPR)
			}
			if len(prs) == opts.ListOptions.PerPage {
				opts.ListOptions.Page += 1
			} else {
				break
			}
		}
	}
}

func (self *Brain) MergePRs(reviews []Review) {
	for _, review := range reviews {
		if review.Condition.Passed {
			log.Printf("Merging %v!", review.PullRequest.ID())
		}
	}
}

func (self *Brain) ReviewPRs() []Review {

	log.Printf("Reviewing all PRs")

	ret := make([]Review, 0)

	for _, repo := range *self.repos.List() {
		prs := self.memory.GetPullRequests(&repo)

		for _, pr := range prs {
			review := self.ReviewPR(&pr)
			ret = append(ret, review)
		}
	}

	return ret
}

func (self *Brain) GetPRs() []PullRequest {
	ret := make([]PullRequest, 0)
	for _, repo := range *self.repos.List() {
		prs := self.memory.GetPullRequests(&repo)
		for _, pr := range prs {
			ret = append(ret, pr)
		}
	}
	return ret
}

func (self *Brain) GetPR(repo *Repo, num int) *PullRequest {
	return self.memory.GetPullRequest(repo, num)
}

func (self *Brain) ReviewPR(pr *PullRequest) Review {

	_, err := self.lock.Lock(pr.ID(), 5000)

	if err != nil {
		panic(err)
	}

	log.Printf("Reviewing %v", pr.ID())

	comments, _, _ := self.client.Issues.ListComments(*pr.Repository.Owner,
		*pr.Repository.Name, pr.Number, nil)

	buildStatuses, _, _ := self.client.Repositories.GetCombinedStatus(*pr.Repository.Owner,
		*pr.Repository.Name, pr.SHA, nil)

	review := Review{
		PullRequest:   pr,
		Comments:      comments,
		BuildStatuses: buildStatuses,
		Condition: Condition{
			Name:   "Overall",
			Passed: true,
		},
	}

	review.Condition.Add(ReviewLGTMs(review))
	review.Condition.Add(ReviewCommands(review))
	review.Condition.Add(ReviewBuildStatus(review))

	if review.Condition.Passed {
		review.Condition.Message = "All conditions met"
	} else {
		review.Condition.Message = "Not all conditions are met"
	}

	self.lock.UnLock()

	return review
}
