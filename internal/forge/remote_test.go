package forge

import "testing"

func TestParseRemote(t *testing.T) {
	tests := []struct {
		name      string
		remote    string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		// SSH scp-like (the default GitHub SSH remote shape).
		{name: "ssh scp with .git", remote: "git@github.com:acme/widget.git", wantOwner: "acme", wantRepo: "widget"},
		{name: "ssh scp no .git", remote: "git@github.com:acme/widget", wantOwner: "acme", wantRepo: "widget"},
		{name: "ssh scp trailing slash", remote: "git@github.com:acme/widget/", wantOwner: "acme", wantRepo: "widget"},
		{name: "ssh scp leading slash in path", remote: "git@github.com:/acme/widget.git", wantOwner: "acme", wantRepo: "widget"},

		// ssh:// URL form.
		{name: "ssh url with .git", remote: "ssh://git@github.com/acme/widget.git", wantOwner: "acme", wantRepo: "widget"},
		{name: "ssh url no .git", remote: "ssh://git@github.com/acme/widget", wantOwner: "acme", wantRepo: "widget"},
		{name: "ssh url with port", remote: "ssh://git@github.com:22/acme/widget.git", wantOwner: "acme", wantRepo: "widget"},
		{name: "ssh url trailing slash", remote: "ssh://git@github.com/acme/widget/", wantOwner: "acme", wantRepo: "widget"},

		// HTTPS / HTTP.
		{name: "https with .git", remote: "https://github.com/acme/widget.git", wantOwner: "acme", wantRepo: "widget"},
		{name: "https no .git", remote: "https://github.com/acme/widget", wantOwner: "acme", wantRepo: "widget"},
		{name: "https trailing slash", remote: "https://github.com/acme/widget/", wantOwner: "acme", wantRepo: "widget"},
		{name: "http no .git", remote: "http://github.com/acme/widget", wantOwner: "acme", wantRepo: "widget"},
		{name: "https with embedded credential", remote: "https://user:tok@github.com/acme/widget.git", wantOwner: "acme", wantRepo: "widget"},

		// Normalization corners.
		{name: "leading/trailing whitespace", remote: "  git@github.com:acme/widget.git\n", wantOwner: "acme", wantRepo: "widget"},
		{name: "mixed-case host", remote: "https://GitHub.com/acme/widget.git", wantOwner: "acme", wantRepo: "widget"},
		{name: "repo name keeps case and dashes", remote: "git@github.com:Acme-Org/My_Repo.git", wantOwner: "Acme-Org", wantRepo: "My_Repo"},
		{name: "dot in repo name is not a suffix", remote: "https://github.com/acme/my.widget", wantOwner: "acme", wantRepo: "my.widget"},

		// Invalid / non-GitHub.
		{name: "empty", remote: "", wantErr: true},
		{name: "whitespace only", remote: "   ", wantErr: true},
		{name: "non-github host https", remote: "https://gitlab.com/acme/widget.git", wantErr: true},
		{name: "non-github host ssh", remote: "git@bitbucket.org:acme/widget.git", wantErr: true},
		{name: "github enterprise host", remote: "https://github.example.com/acme/widget.git", wantErr: true},
		{name: "missing repo segment", remote: "https://github.com/acme", wantErr: true},
		{name: "too many path segments", remote: "https://github.com/acme/widget/extra", wantErr: true},
		{name: "no path at all", remote: "https://github.com", wantErr: true},
		{name: "bare path no host", remote: "acme/widget.git", wantErr: true},
		{name: "garbage", remote: "not a url", wantErr: true},
		{name: "unsupported scheme", remote: "ftp://github.com/acme/widget.git", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, err := ParseRemote(tt.remote)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseRemote(%q) = (%q, %q, nil); want error", tt.remote, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRemote(%q) error: %v", tt.remote, err)
			}
			if owner != tt.wantOwner || repo != tt.wantRepo {
				t.Errorf("ParseRemote(%q) = (%q, %q); want (%q, %q)", tt.remote, owner, repo, tt.wantOwner, tt.wantRepo)
			}
		})
	}
}

// A credential embedded in an HTTPS remote must never appear in the returned
// values nor in any error — secrets stay out of logs and out of the model (I3).
func TestParseRemoteDoesNotLeakCredential(t *testing.T) {
	owner, repo, err := ParseRemote("https://user:s3cr3t-tok@github.com/acme/widget.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if owner != "acme" || repo != "widget" {
		t.Fatalf("got (%q, %q); want (acme, widget)", owner, repo)
	}

	// And on a malformed credentialed remote the error text must not echo the token.
	_, _, err = ParseRemote("https://user:s3cr3t-tok@gitlab.com/acme/widget.git")
	if err == nil {
		t.Fatal("want error for non-github host")
	}
	// The full remote (with token) is intentionally not surfaced; only host/segments are.
	// Guard the specific secret value regardless.
	if got := err.Error(); containsSecret(got) {
		t.Errorf("error leaked credential: %v", got)
	}
}

func containsSecret(s string) bool {
	const secret = "s3cr3t-tok"
	for i := 0; i+len(secret) <= len(s); i++ {
		if s[i:i+len(secret)] == secret {
			return true
		}
	}
	return false
}

func TestDefaultBase(t *testing.T) {
	if got := DefaultBase(); got != "main" {
		t.Errorf("DefaultBase() = %q; want %q", got, "main")
	}
}
