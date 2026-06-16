package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type approver struct{ ok bool }

func (a approver) Approve(string) bool { return a.ok }

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{Token: "tok-secret", BaseURL: srv.URL, HTTP: srv.Client()}
}

func TestOpenSendsDraftPRWithAuth(t *testing.T) {
	var gotAuth, gotBody string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/pulls" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/acme/widget/pull/7"}`))
	})

	url, err := c.Open(context.Background(), PR{Owner: "acme", Repo: "widget", Head: "task/x", Base: "main", Title: "T", Body: "B", Draft: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if url != "https://github.com/acme/widget/pull/7" {
		t.Errorf("url = %q", url)
	}
	if gotAuth != "Bearer tok-secret" {
		t.Errorf("auth header = %q", gotAuth)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if body["draft"] != true {
		t.Errorf("draft not set: %v", body["draft"])
	}
}

func TestGatedOpenApprovedRunsPrepareThenOpens(t *testing.T) {
	prepared := false
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if !prepared {
			t.Error("Open called before prepare")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"html_url":"https://x/pull/1"}`))
	})
	url, opened, err := c.GatedOpen(context.Background(), approver{ok: true},
		PR{Owner: "a", Repo: "b", Head: "task/x", Base: "main", Title: "t", Draft: true},
		func(context.Context) error { prepared = true; return nil })
	if err != nil || !opened || url == "" {
		t.Fatalf("gated open approved: url=%q opened=%v err=%v", url, opened, err)
	}
}

func TestGatedOpenDeniedDoesNotCall(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("Open must not be called when the gate denies")
	})
	called := false
	url, opened, err := c.GatedOpen(context.Background(), approver{ok: false},
		PR{Owner: "a", Repo: "b", Head: "task/x", Base: "main"},
		func(context.Context) error { called = true; return nil })
	if opened || url != "" || err != nil {
		t.Fatalf("denied gate: url=%q opened=%v err=%v", url, opened, err)
	}
	if called {
		t.Error("prepare ran despite a denied gate")
	}
}

func TestGatedOpenNilApproverDenies(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("Open must not be called with a nil approver")
	})
	_, opened, err := c.GatedOpen(context.Background(), nil, PR{Owner: "a", Repo: "b", Head: "h", Base: "main"}, nil)
	if opened || err != nil {
		t.Fatalf("nil approver must default-deny: opened=%v err=%v", opened, err)
	}
}

func TestOpenErrorDoesNotLeakToken(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	})
	_, err := c.Open(context.Background(), PR{Owner: "a", Repo: "b", Head: "h", Base: "main"})
	if err == nil {
		t.Fatal("want error on 403")
	}
	if strings.Contains(err.Error(), "tok-secret") {
		t.Errorf("error leaked the token: %v", err)
	}
}
