package main

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

//go:embed index.tmpl.html
var indexContents string
var indexTemplate = template.Must(template.New("index.html").Parse(indexContents))

type server struct {
	db *sql.DB
	// Used when no explicit branch is requested.
	defaultBranch string
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.Redirect(w, r, "/", http.StatusPermanentRedirect)
		return
	}

	if err := s.serveBoard(w, r); err != nil {
		log.Printf("request handler error: %s", err)
		http.Error(w, "internal error; see logs for details", http.StatusInternalServerError)
		return
	}
}

func (s *server) serveBoard(w http.ResponseWriter, r *http.Request) error {
	repoLock.RLock()
	defer repoLock.RUnlock()

	var re repo
	if s := r.URL.Query().Get("repo"); s != "" {
		repoID, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		for _, re0 := range repos {
			if re0.id == int64(repoID) {
				re = re0
			}
		}
	} else if len(repos) > 0 {
		re = repos[0]
	} else {
		return errors.New("no repos available")
	}

	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = s.defaultBranch
	}
	branchOk := false
	for _, b := range re.releaseBranches {
		if b == branch {
			branchOk = true
			break
		}
	}
	if !branchOk {
		return fmt.Errorf("%q is not a release branch", branch)
	}

	var commits []commit
	if sha, ok := re.branchMergeBases[branch]; ok {
		commits = re.masterCommits.truncate(sha)
	} else {
		return fmt.Errorf("unknown branch %q", branch)
	}

	authors := map[user]struct{}{}
	for _, c := range commits {
		authors[c.Author] = struct{}{}
	}
	var author user
	if s := r.URL.Query().Get("author"); s != "" {
		for a := range authors {
			if a.Email == s {
				author = a
			}
		}
		if author == (user{}) {
			return fmt.Errorf("%q is not a recognized author", author)
		}
		var newCommits []commit
		for _, c := range commits {
			if c.Author == author {
				newCommits = append(newCommits, c)
			}
		}
		commits = newCommits
	}
	var sortedAuthors []user
	for a := range authors {
		sortedAuthors = append(sortedAuthors, a)
	}
	sort.Slice(sortedAuthors, func(i, j int) bool {
		return strings.Compare(sortedAuthors[i].Email, sortedAuthors[j].Email) < 0
	})

	labels := map[string]struct{}{}
	for _, pr := range re.masterPRs {
		for _, l := range pr.labels {
			labels[l] = struct{}{}
		}
	}
	var label string
	if s := r.URL.Query().Get("label"); s != "" {
		for l := range labels {
			if l == s {
				label = s
				break
			}
		}
		if label == "" {
			return fmt.Errorf("%q is not a recognized label", s)
		}
		var newCommits []commit
		for _, c := range commits {
			masterPR, ok := re.masterPRs[string(c.sha)]
			if !ok {
				return fmt.Errorf("%s is not a commit on the master branch", c.sha)
			}
			matchingLabel := false
			for _, l := range masterPR.labels {
				if l == label {
					matchingLabel = true
					break
				}
			}
			if matchingLabel {
				newCommits = append(newCommits, c)
			}
		}
		commits = newCommits
	}
	var sortedLabels []string
	for l := range labels {
		sortedLabels = append(sortedLabels, l)
	}
	sort.Slice(sortedLabels, func(i, j int) bool {
		return strings.Compare(sortedLabels[i], sortedLabels[j]) < 0
	})

	masterPRs := map[int][]string{}
	var acommits []acommit
	var lastMasterPR *pr
	masterPRStart := -1
	var lastBackportPR *pr
	backportPRStart := -1
	for i, c := range commits {
		var oldestTags []string
		if len(c.oldestTag) > 0 {
			oldestTags = append(oldestTags, c.oldestTag)
		}
		// TODO(benesch): these rowspan computations hurt to look at.
		masterPR := re.masterPRs[string(c.sha)]
		if masterPR == nil {
			continue
		}
		// TODO(benesch): masterPR should never be nil!
		if masterPR != nil && (lastMasterPR == nil || lastMasterPR.number != masterPR.number) {
			if masterPRStart >= 0 && masterPRStart < len(acommits) {
				acommits[masterPRStart].MasterPRRowSpan = i - masterPRStart
			}
			masterPRStart = i
			lastMasterPR = masterPR
		}
		backportPR := re.branchPRs[c.MessageID()][branch]
		if !((lastBackportPR == nil && backportPR == nil && lastMasterPR != masterPR) || (lastBackportPR != nil && backportPR != nil && lastBackportPR.number == backportPR.number)) {
			if backportPRStart >= 0 && backportPRStart < len(acommits) {
				acommits[backportPRStart].BackportPRRowSpan = i - backportPRStart
			}
			backportPRStart = i
			lastBackportPR = backportPR
		}

		var backportStatus string
		if backportPR != nil {
			if backportPR.mergedAt.Valid {
				backportStatus = "✓"
			} else {
				backportStatus = "◷"
			}
		}
		// TODO(benesch): redundant. which to keep?
		if _, backported := re.branchCommits[branch].messageIDs[c.MessageID()]; backported {
			backportStatus = "✓"
		}
		// Get tags from all release branches. not just the currently selected
		// one. This makes it easier for CS and TSE to know whether a commit
		// has been released yet.
		for _, releaseBranch := range re.releaseBranches {
			if cIdx, backported := re.branchCommits[releaseBranch].messageIDs[c.MessageID()]; backported {
				backportCommit := re.branchCommits[releaseBranch].commits[cIdx]
				if backportCommit.oldestTag != "" {
					oldestTags = append(oldestTags, backportCommit.oldestTag)
				}
			}
		}
		acommits = append(acommits, acommit{
			commit:         c,
			BackportStatus: backportStatus,
			MasterPR:       masterPR,
			BackportPR:     backportPR,
			Backportable:   backportPR == nil,
			oldestTags:     oldestTags,
		})
		masterPRs[masterPR.number] = append(masterPRs[masterPR.number], c.sha.String())
	}
	if masterPRStart >= 0 && masterPRStart < len(acommits) {
		acommits[masterPRStart].MasterPRRowSpan = len(acommits) - masterPRStart
	}
	if backportPRStart >= 0 && backportPRStart < len(acommits) {
		acommits[backportPRStart].BackportPRRowSpan = len(acommits) - backportPRStart
	}

	if err := indexTemplate.Execute(w, struct {
		Repos     []repo
		Repo      repo
		Commits   []acommit
		Branches  []string
		Branch    string
		Authors   []user
		Author    user
		Labels    []string
		Label     string
		MasterPRs map[int][]string
	}{
		Repos:     repos,
		Repo:      re,
		Commits:   acommits,
		Branches:  re.releaseBranches,
		Branch:    branch,
		Authors:   sortedAuthors,
		Author:    author,
		Labels:    sortedLabels,
		Label:     label,
		MasterPRs: masterPRs,
	}); err != nil {
		return err
	}
	return nil
}
