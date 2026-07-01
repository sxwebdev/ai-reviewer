package gitlab

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// MRRef identifies a merge request. Exactly one of ProjectPath / ProjectID is
// set. Host is set only for the URL form; otherwise the caller supplies the
// configured host.
type MRRef struct {
	Host        string
	ProjectPath string
	ProjectID   int64
	IID         int64
}

// ProjectKey returns the URL-encoded project identifier for API paths: either
// the numeric id or the URL-encoded "group/repo" path.
func (r MRRef) ProjectKey() string {
	if r.ProjectID != 0 {
		return strconv.FormatInt(r.ProjectID, 10)
	}
	return url.PathEscape(r.ProjectPath)
}

var idColonIID = regexp.MustCompile(`^(\d+):(\d+)$`)

// ParseRef parses one of:
//
//	https://host/group/repo/-/merge_requests/123   (URL)
//	group/subgroup/repo!123                         (path!iid)
//	42:123                                          (project-id:iid)
//
// defaultHost is used for the non-URL forms.
func ParseRef(s, defaultHost string) (MRRef, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return MRRef{}, fmt.Errorf("empty MR reference")
	}

	switch {
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
		return parseURLRef(s)

	case idColonIID.MatchString(s):
		m := idColonIID.FindStringSubmatch(s)
		pid, _ := strconv.ParseInt(m[1], 10, 64)
		iid, _ := strconv.ParseInt(m[2], 10, 64)
		return MRRef{Host: defaultHost, ProjectID: pid, IID: iid}, nil

	case strings.Contains(s, "!"):
		i := strings.LastIndex(s, "!")
		path := strings.Trim(s[:i], "/")
		iid, err := strconv.ParseInt(s[i+1:], 10, 64)
		if err != nil {
			return MRRef{}, fmt.Errorf("invalid iid in %q: %w", s, err)
		}
		if path == "" {
			return MRRef{}, fmt.Errorf("missing project path in %q", s)
		}
		return MRRef{Host: defaultHost, ProjectPath: path, IID: iid}, nil

	default:
		return MRRef{}, fmt.Errorf("unrecognized MR reference %q", s)
	}
}

func parseURLRef(raw string) (MRRef, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return MRRef{}, fmt.Errorf("parse url %q: %w", raw, err)
	}
	host := u.Scheme + "://" + u.Host

	path := strings.Trim(u.Path, "/")
	// Accept both "/-/merge_requests/" and "/merge_requests/".
	marker := "/-/merge_requests/"
	idx := strings.Index(path, marker)
	if idx < 0 {
		marker = "/merge_requests/"
		idx = strings.Index(path, marker)
	}
	if idx < 0 {
		return MRRef{}, fmt.Errorf("url %q is not a merge request URL", raw)
	}
	project := path[:idx]
	rest := path[idx+len(marker):]
	// The iid is the first path segment after the marker.
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	iid, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return MRRef{}, fmt.Errorf("invalid iid in url %q: %w", raw, err)
	}
	if project == "" {
		return MRRef{}, fmt.Errorf("missing project path in url %q", raw)
	}
	return MRRef{Host: host, ProjectPath: project, IID: iid}, nil
}
