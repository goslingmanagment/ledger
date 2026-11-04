package journal

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/goslingmanagment/ledger/internal/ts"
)

// FromGit builds the bundle from the git history of path in repo: one version per commit that touched
// the file, carrying the rows new or changed since the previous commit, recorded at the commit time.
// With worktree, an uncommitted working copy that differs from HEAD becomes a last version.
func FromGit(ctx context.Context, repo, path string, worktree bool) (Bundle, error) {
	b := Bundle{Source: "git:" + path}
	out, err := gitOut(ctx, repo, "log", "--reverse", "--format=%H %cI", "--", path)
	if err != nil {
		return b, err
	}
	prev := map[int]string{}
	var text, ref, at string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		sha, when, _ := strings.Cut(line, " ")
		t, err := time.Parse(time.RFC3339, when)
		if err != nil {
			return b, fmt.Errorf("commit %s: time %q", sha, when)
		}
		text, err = gitOut(ctx, repo, "show", sha+":"+path)
		if err != nil {
			return b, err
		}
		ref, at = sha[:8], ts.Format(t)
		b.Versions = append(b.Versions, delta(text, prev, ref, at))
	}
	if worktree {
		raw, err := os.ReadFile(filepath.Join(repo, path))
		if err != nil {
			return b, err
		}
		if string(raw) != text {
			st, _ := os.Stat(filepath.Join(repo, path))
			text, ref, at = string(raw), "worktree", ts.Format(st.ModTime())
			b.Versions = append(b.Versions, delta(text, prev, ref, at))
		}
	}
	b.Final, b.FinalRef, b.FinalAt = text, ref, at
	return b, nil
}

// FromFile is a bundle of one version: the file as it is now.
func FromFile(text, ref string, at time.Time) Bundle {
	v := delta(text, map[int]string{}, ref, ts.Format(at))
	return Bundle{Source: "file", Versions: []Version{v}, Final: text, FinalRef: ref, FinalAt: ts.Format(at)}
}

func delta(text string, prev map[int]string, ref, at string) Version {
	v := Version{Ref: ref, At: at}
	f, _ := Parse(text)
	for _, r := range f.Rows {
		line := r.Format()
		if prev[r.Num] != line {
			v.Rows = append(v.Rows, r.Cells())
			prev[r.Num] = line
		}
	}
	return v
}

func gitOut(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
