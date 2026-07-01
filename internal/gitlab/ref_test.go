package gitlab

import "testing"

func TestParseRef(t *testing.T) {
	const host = "https://gitlab.example.com"
	tests := []struct {
		name    string
		in      string
		want    MRRef
		wantErr bool
	}{
		{
			name: "url with -/",
			in:   "https://gitlab.example.com/group/repo/-/merge_requests/123",
			want: MRRef{Host: "https://gitlab.example.com", ProjectPath: "group/repo", IID: 123},
		},
		{
			name: "url nested groups",
			in:   "https://gl.corp/team/sub/proj/-/merge_requests/7",
			want: MRRef{Host: "https://gl.corp", ProjectPath: "team/sub/proj", IID: 7},
		},
		{
			name: "url without -/",
			in:   "https://gitlab.example.com/group/repo/merge_requests/9",
			want: MRRef{Host: "https://gitlab.example.com", ProjectPath: "group/repo", IID: 9},
		},
		{
			name: "url with trailing segment",
			in:   "https://gitlab.example.com/group/repo/-/merge_requests/12/diffs",
			want: MRRef{Host: "https://gitlab.example.com", ProjectPath: "group/repo", IID: 12},
		},
		{
			name: "path bang iid",
			in:   "group/sub/repo!42",
			want: MRRef{Host: host, ProjectPath: "group/sub/repo", IID: 42},
		},
		{
			name: "project id colon iid",
			in:   "1024:55",
			want: MRRef{Host: host, ProjectID: 1024, IID: 55},
		},
		{name: "empty", in: "", wantErr: true},
		{name: "garbage", in: "not-a-ref", wantErr: true},
		{name: "bang no iid", in: "group/repo!abc", wantErr: true},
		{name: "url not mr", in: "https://gitlab.example.com/group/repo", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRef(tt.in, host)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestProjectKey(t *testing.T) {
	if k := (MRRef{ProjectID: 42}).ProjectKey(); k != "42" {
		t.Errorf("id key = %q", k)
	}
	if k := (MRRef{ProjectPath: "group/repo"}).ProjectKey(); k != "group%2Frepo" {
		t.Errorf("path key = %q, want url-encoded", k)
	}
}
