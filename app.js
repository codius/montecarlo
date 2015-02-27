var bluebird = require('bluebird');
var moment = require('moment');
var express = require('express');
var app = express();
var reviewer = require('./lib/reviewer');
var reviewers = require('./lib/reviewers');
var bodyParser = require('body-parser');
var PullRequestQueue = require('./lib/review-queue').PullRequestQueue;
var tracker = require('./lib/tracker');
var kue = require('./lib/kue');
var github = require('./lib/github');
var nconf = require('./lib/config');
var redis = require('./lib/redis');
var travis = require('./lib/travis');
var circleci = require('./lib/circleci');

var project = tracker.project(process.env.TRACKER_PROJECT_ID);
var queue = new PullRequestQueue(kue, github, project);
queue.addReviewerFactory(function(r) {
  return new reviewers.LGTMProcessor(r, nconf.get('reviewers:lgtm:threshold'));
});
queue.addReviewerFactory(function(r) {
  return new reviewers.TrackerProcessor(r, project);
});
queue.addReviewerFactory(function(r) {
  return new reviewers.WebuiStateProcessor(r, redis);
});

app.set('port', (process.env.PORT || 5000));
app.set('view engine', 'jade');
app.use(express.static(__dirname + '/public'));
app.use(bodyParser.json());

app.get('/', function(req, res) {
  bluebird.join(
    redis.smembersAsync("pull-requests").map(Number).then(function(ids) {
      var p = [];
      ids.forEach(function(id) {
        p.push(redis.hgetallAsync('pr:'+id));
      });
      return bluebird.all(p);
    }),
    redis.hgetAsync("crawl-state", "last-run"),
    redis.hgetAsync("crawl-state", "running"),
    circleci.getProjects(),
    function(reqs, lastRun, isRunning, circleProjects) {
      var buildStatus = [];
      circleProjects.forEach(function(r) {
        if (r.vcs_url.indexOf('/codius/') != -1) {
          console.log(r.branches.master.recent_builds);
          var build;
          if (r.branches.integration) {
            build = r.branches.integration.recent_builds[0];
          } else if (r.branches.develop) {
            build = r.branches.develop.recent_builds[0];
          } else {
            build = r.branches.master.recent_builds[0];
          }
          buildStatus.push({
            slug: 'codius/'+r.reponame,
            project_url: 'https://circleci.com/gh/codius/'+r.reponame,
            build_url: 'https://circleci.com/gh/codius/'+r.reponame+'/'+build.build_num,
            state: build.outcome
          });
        }
      });
      var queue = {merged: [], open: [], closed: []};
      reqs.forEach(function(r) {
        if (r) {
          if (r.state == "merged") {
            queue.merged.push(r);
          } else if (r.state == "open") {
            queue.open.push(r);
          } else if (r.state == "closed") {
            queue.closed.push(r);
          }
        }
      });
      res.render('index', {
        queue: queue,
        lastRun: moment(lastRun).format('MMM Do YY, h:mm:ss a'),
        isRunning: isRunning,
        buildStatus: buildStatus
      });
    }
  );
});

app.post('/github-hook', function(req, res) {
  var eventType = req.headers['x-github-event'];
  console.log("Handling github hook: %s", eventType);
  console.log(req.body);
  if (eventType == 'status') {
    queue.enqueuePullRequest(
      req.body.repository.owner.login,
      req.body.repository.name,
      -1
    );
    res.send("Reviewing");
  } else if (eventType == 'issue_comment') {
    queue.enqueuePullRequest(
      req.body.repository.owner.login,
      req.body.repository.name,
      req.body.issue.number
    );
    res.send("Reviewing");
  } else if (eventType == 'pull_request') {
    if (req.body.action == "opened" || req.body.action == "reopened" || req.body.action == "closed") {
      queue.enqueuePullRequest(
        req.body.pull_request.base.repo.owner.login,
        req.body.pull_request.base.repo.name,
        req.body.pull_request.number
      );
    }
    res.send("Handling pull request");
  } else if (eventType == 'push') {
    queue.enqueuePullRequest(
      req.body.repository.owner.login,
      req.body.repository.name,
      -1
    );
    res.send("Reviewing");
  } else {
    res.send("Unknown event: " + JSON.stringify(req.body));
  }
});

app.get('/crawl', function(req, res) {
  github.user.getTeamsAsync({}).then(function(teams) {
    var p = [];
    teams.forEach(function(team) {
      p.push(github.orgs.getTeamReposAsync({
        id: team.id
      }).then(function(repos) {
        var p = [];
        repos.forEach(function(repo) {
          p.push(queue.enqueuePullRequest(repo.owner.login, repo.name, -1));
        });
        return bluebird.all(p);
      }));
    });
    return bluebird.all(p);
  }).then(function() {
    res.send("Running crawler on repos!");
  });
});

module.exports = app;