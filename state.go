package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach-go/crdb"
	"github.com/google/go-github/github"
	"github.com/lib/pq"
)

const schema = `
CREATE TABLE IF NOT EXISTS repos (
	id serial PRIMARY KEY,
	github_owner string NOT NULL,
	github_repo string NOT NULL,
	UNIQUE (github_owner, github_repo)
);

CREATE TABLE IF NOT EXISTS prs (
	id int PRIMARY KEY,
	repo_id int REFERENCES repos,
	number int,
	title string,
	body string,
	open bool,
	merged_at timestamptz,
	base_sha bytes,
	base_branch string,
	author_username string,
	updated_at timestamptz,
	UNIQUE (repo_id, number)
);

CREATE TABLE IF NOT EXISTS pr_commits (
	pr_id int REFERENCES prs,
	sha bytes,
	title string,
	body string,
	message_id bytes,
	author_email string,
	ordering int,
	PRIMARY KEY (pr_id, sha)
);

CREATE TABLE IF NOT EXISTS pr_labels (
    pr_id int REFERENCES prs,
    label string,
    PRIMARY KEY (pr_id, label)
);

CREATE TABLE IF NOT EXISTS exclusions (
	message_id bytes PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS commit_comments (
	message_id bytes,
	created_at timestamptz,
	sha bytes,
	user_email string,
	body string,
	PRIMARY KEY (message_id, created_at)
);`

// TODO(benesch): ewww
var repoLock sync.RWMutex

type repo struct {
	id          int64
	githubOwner string
	githubRepo  string

	releaseBranches []string

	masterCommits    commits
	branchCommits    map[string]commits
	branchMergeBases map[string]sha

	masterPRs map[string]*pr            // by SHA
	branchPRs map[string]map[string]*pr // by message ID

	lastRefresh time.Time
}

func (r repo) path() string {
	return filepath.Join("repos", r.githubRepo)
}

func (r repo) url() string {
	return "https://github.com/" + path.Join(r.githubOwner, r.githubRepo) + ".git"
}

func (r *repo) refresh(db *sql.DB) error {
	cs, err := loadCommits(*r, "master")
	if err != nil {
		return err
	}
	r.masterCommits = cs
	r.branchCommits = map[string]commits{}
	r.branchMergeBases = map[string]sha{}
	for _, branch := range r.releaseBranches {
		cs, err = loadCommits(*r, branch, "^master")
		if err != nil {
			return err
		}
		r.branchCommits[branch] = cs
		out, err := capture("git", "-C", r.path(), "merge-base", "master", branch)
		if err != nil {
			return err
		}
		r.branchMergeBases[branch], err = parseSHA(out)
		if err != nil {
			return err
		}
	}

	// TODO(benesch): what if multiple PRs have the same commit?

	r.masterPRs = map[string]*pr{}
	rows, err := db.Query(
		`SELECT number, merged_at, sha, array_agg(prl.label)
		FROM pr_commits JOIN prs ON pr_commits.pr_id = prs.id
        LEFT JOIN pr_labels prl ON prs.id = prl.pr_id
		WHERE merged_at IS NOT NULL AND base_branch = 'master'
		GROUP BY number, merged_at, sha`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		pr := &pr{repo: r}
		var tmp []sql.NullString
		if err := rows.Scan(&pr.number, &pr.mergedAt, &s, pq.Array(&tmp)); err != nil {
			return err
		}
		for _, t := range tmp {
			if t.Valid {
				pr.labels = append(pr.labels, t.String)
			}
		}
		r.masterPRs[s] = pr
	}
	if err := rows.Err(); err != nil {
		return err
	}

	r.branchPRs = map[string]map[string]*pr{}
	rows, err = db.Query(
		`SELECT number, merged_at, message_id, base_branch, array_agg(prl.label)
		FROM pr_commits JOIN prs ON pr_commits.pr_id = prs.id LEFT JOIN pr_labels prl ON prs.id = prl.pr_id
		WHERE merged_at IS NOT NULL OR open
		GROUP BY number, merged_at, message_id, base_branch`)

	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var baseBranch string
		p := &pr{repo: r}
		var tmp []sql.NullString
		if err := rows.Scan(&p.number, &p.mergedAt, &messageID, &baseBranch, pq.Array(&tmp)); err != nil {
			return err
		}
		for _, t := range tmp {
			if t.Valid {
				p.labels = append(p.labels, t.String)
			}
		}
		if r.branchPRs[messageID] == nil {
			r.branchPRs[messageID] = map[string]*pr{}
		}
		r.branchPRs[messageID][baseBranch] = p
	}
	if err := rows.Err(); err != nil {
		return err
	}
	r.lastRefresh = time.Now().UTC()
	return nil
}

func (r repo) ID() int64 {
	return r.id
}

func (r repo) LastRefresh() time.Time {
	return r.lastRefresh
}

func (r repo) String() string {
	return r.githubOwner + "/" + r.githubRepo
}

type commits struct {
	commits    []commit
	shas       map[string]struct{}
	messageIDs map[string]struct{}
}

func (cs *commits) insert(c commit) {
	cs.commits = append(cs.commits, c)
	if cs.shas == nil {
		cs.shas = map[string]struct{}{}
	}
	if cs.messageIDs == nil {
		cs.messageIDs = map[string]struct{}{}
	}
	cs.shas[string(c.sha)] = struct{}{}
	cs.messageIDs[c.MessageID()] = struct{}{}
}

func (cs commits) subtract(cs0 commits) []commit {
	var out []commit
	for _, c := range cs.commits {
		if _, ok := cs0.shas[string(c.sha)]; !ok {
			out = append(out, c)
		}
	}
	return out
}

func (cs commits) truncate(sha sha) []commit {
	var out []commit
	for _, cs := range cs.commits {
		if string(cs.sha) == string(sha) {
			break
		}
		if !cs.merge {
			out = append(out, cs)
		}
	}
	return out
}

type user struct {
	Email string
}

func (u user) Short() string {
	return strings.Split(u.Email, "@")[0]
}

func (u user) String() string {
	return u.Email
}

type commit struct {
	sha        sha
	CommitDate time.Time
	Author     user
	title      string
	body       string
	merge      bool
}

func (c commit) SHA() sha {
	return c.sha
}

func (c commit) Title() string {
	return c.title
}

func (c commit) MessageID() string {
	h := sha1.New()
	io.WriteString(h, c.title)
	io.WriteString(h, c.body)
	return string(h.Sum(nil))
}

// "annotated" commit; this belongs elsewhere (it's templating logic)
type acommit struct {
	commit
	Backportable      bool
	BackportStatus    string
	MasterPR          *pr
	MasterPRRowSpan   int
	BackportPR        *pr
	BackportPRRowSpan int
}

const commitFormat = "%H%x00%s%x00%cI%x00%aE%x00%P"

func loadCommits(re repo, constraints ...string) (cs commits, err error) {
	args := []string{
		"git", "-C", re.path(), "log", "--topo-order", "--format=format:" + commitFormat,
	}
	args = append(args, constraints...)
	out, err := capture(args...)
	if err != nil {
		return commits{}, err
	}
	// TODO(benesch): stream this?
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\x00")
		sha, err := parseSHA(fields[0])
		if err != nil {
			return commits{}, err
		}
		commitDate, err := time.Parse(time.RFC3339, fields[2])
		if err != nil {
			return commits{}, err
		}
		authorEmail := fields[3]
		cs.insert(commit{
			sha:        sha,
			CommitDate: commitDate,
			Author:     user{authorEmail},
			title:      fields[1],
			merge:      strings.Count(fields[4], " ") > 0,
		})
	}
	return cs, err
}

type sha []byte

func parseSHA(s string) (sha, error) {
	shaBytes, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(shaBytes) != 20 {
		return nil, fmt.Errorf("corrupt sha (%d bytes intead of 20)", len(shaBytes))
	}
	return sha(shaBytes), nil
}

func (s sha) Short() string {
	return s.String()[0:9]
}

func (s sha) String() string {
	return hex.EncodeToString(s)
}

type pr struct {
	repo     *repo
	number   int
	mergedAt pq.NullTime
	labels   []string
}

func (p *pr) Number() int {
	return p.number
}

func (p *pr) String() string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("#%d", p.number)
}

func (p *pr) URL() string {
	if p == nil {
		return "#"
	}
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d",
		p.repo.githubOwner, p.repo.githubRepo, p.number)
}

func (p *pr) MergedAt() string {
	if p.mergedAt.Valid {
		return p.mergedAt.Time.Format("2006-01-02 15:04:05")
	}
	return "(unknown)"
}

func (p *pr) Labels() string {
	return strings.Join(p.labels, ", ")
}

func syncAll(ctx context.Context, ghClient *github.Client, db *sql.DB) error {
	for i := range repos {
		if err := syncRepo(ctx, ghClient, db, &repos[i]); err != nil {
			return err
		}
	}
	return nil
}

func syncRepo(ctx context.Context, ghClient *github.Client, db *sql.DB, repo *repo) error {
	log.Printf("syncing %s", repo)
	defer log.Printf("done syncing %s", repo)
	if err := spawn("git", "-C", repo.path(), "fetch"); err != nil {
		return err
	}

	opts := &github.PullRequestListOptions{
		State:       "all",
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var allPRs []*github.PullRequest
	for {
		prs, res, err := ghClient.PullRequests.List(ctx, repo.githubOwner, repo.githubRepo, opts)
		if err != nil {
			return err
		}
		allPRs = append(allPRs, prs...)
		log.Printf("fetched %d updated PRs (total: %d)", len(prs), len(allPRs))

		if res.NextPage == 0 {
			break
		}
		lastPR := prs[len(prs)-1]
		if ok, err := isPRUpToDate(ctx, db, lastPR); err != nil {
			return err
		} else if ok {
			break
		}
		opts.Page = res.NextPage
	}

	// process updates from least to most recent
	for i := len(allPRs) - 1; i >= 0; i-- {
		if err := syncPR(ctx, db, repo, allPRs[i]); err != nil {
			if allPRs[i].MergedAt == nil && allPRs[i].GetState() == "closed" {
				log.Printf("ignoring error while syncing closed, unmerged pr %d: %s",
					allPRs[i].GetNumber(), err)
			} else {
				return err
			}
		}
	}

	repoCopy := *repo
	if err := repoCopy.refresh(db); err != nil {
		return err
	}

	repoLock.Lock()
	*repo = repoCopy
	repoLock.Unlock()
	return nil
}

type queryer interface {
	QueryRow(query string, args ...interface{}) *sql.Row
}

func isPRUpToDate(ctx context.Context, q queryer, pr *github.PullRequest) (bool, error) {
	// Support for PR labels was added later, so some PRs that are otherwise
	// up-to-date need be re-synced. This block can be removed at any point
	// in the future, but there isn't much harm in keeping it.
	if len(pr.Labels) != 0 {
		var labelCount int
		err := q.QueryRow(`SELECT count(*) FROM pr_labels WHERE pr_id = $1`, pr.GetID()).Scan(&labelCount)
		if err == sql.ErrNoRows {
			return false, nil
		} else if err != nil {
			return false, err
		}
		if labelCount != len(pr.Labels) {
			return false, nil
		}
	}

	var updatedAt time.Time
	err := q.QueryRow(`SELECT updated_at FROM prs WHERE id = $1`, pr.GetID()).Scan(&updatedAt)
	if err == sql.ErrNoRows {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return updatedAt.Equal(pr.GetUpdatedAt()), nil
}

func syncPR(ctx context.Context, db *sql.DB, repo *repo, pr *github.PullRequest) error {
	log.Printf("pr: %d", pr.GetNumber())

	prBase := pr.GetBase().GetSHA()
	prHead := fmt.Sprintf("refs/pull/%d/head", pr.GetNumber())
	commits, err := loadCommits(*repo, prHead, "^"+prBase)
	if err != nil {
		// Hack for PR 47761, which seems to be missing from GitHub as of 2020-05-19.
		// https://github.com/cockroachdb/dev-inf/issues/100
		if strings.Contains(err.Error(), "exit status 128") {
			log.Printf("skipping PR sync for %d due to missing refspec", pr.GetID())
			return nil
		}
		return err
	}

	return crdb.ExecuteTx(ctx, db, nil /* txopts */, func(tx *sql.Tx) error {
		if ok, err := isPRUpToDate(ctx, tx, pr); err != nil {
			return err
		} else if ok {
			return nil
		}
		if _, err := tx.Exec(
			`UPSERT INTO prs (id, repo_id, number, title, body, open, merged_at, base_sha, base_branch, author_username, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			pr.GetID(), repo.id, pr.GetNumber(),
			pr.GetTitle(), pr.GetBody(),
			pr.GetState() == "open", pr.MergedAt,
			pr.GetBase().GetSHA(), pr.GetBase().GetRef(),
			pr.GetUser().GetLogin(), pr.GetUpdatedAt(),
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM pr_commits WHERE pr_id = $1`, pr.GetID()); err != nil {
			return err
		}
		for i, c := range commits.commits {
			if _, err := tx.Exec(
				`INSERT INTO pr_commits (pr_id, sha, title, body, message_id, author_email, ordering)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				pr.GetID(),
				c.sha,
				c.title,
				c.body,
				c.MessageID(),
				c.Author.Email,
				i,
			); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM pr_labels WHERE pr_id = $1`, pr.GetID()); err != nil {
			return err
		}
		for _, l := range pr.Labels {
			if !strings.HasPrefix(l.GetName(), "backport-") {
				continue
			}
			if _, err := tx.Exec(
				`INSERT INTO pr_labels (pr_id, label) VALUES ($1, $2)`,
				pr.GetID(),
				l.GetName(),
			); err != nil {
				return err
			}
		}
		return nil
	})
}

func bootstrap(ctx context.Context, db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	for i := range repos {
		var id int64
		if err := db.QueryRowContext(
			ctx,
			`INSERT INTO repos (github_owner, github_repo)
			VALUES ($1, $2)
			ON CONFLICT (github_owner, github_repo) DO UPDATE SET github_owner = excluded.github_owner
			RETURNING id`,
			repos[i].githubOwner, repos[i].githubRepo,
		).Scan(&id); err != nil {
			return err
		}
		repos[i].id = id

		url, path := repos[i].url(), repos[i].path()
		if _, err := os.Stat(path); os.IsNotExist(err) {
			log.Printf("cloning %s into %s", repos[i], path)
			if err := spawn("git", "clone", "--mirror", url, path); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}

		out, err := capture("git", "-C", path, "branch", "--list", "release-*")
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(strings.NewReader(out))
		for scanner.Scan() {
			repos[i].releaseBranches = append(repos[i].releaseBranches, strings.TrimSpace(scanner.Text()))
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		sort.Sort(sort.Reverse(sort.StringSlice(repos[i].releaseBranches)))

		if err := repos[i].refresh(db); err != nil {
			return err
		}
	}
	return nil
}
