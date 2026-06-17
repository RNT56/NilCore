package tools

import (
	"context"
	"encoding/json"
	"testing"

	"nilcore/internal/sandbox"
)

// plainTool implements only Tool (not ImageRunner), to prove DispatchRich returns
// a nil image for ordinary tools (byte-identical to Dispatch).
type plainTool struct{}

func (plainTool) Name() string            { return "plain" }
func (plainTool) Description() string     { return "d" }
func (plainTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (plainTool) Run(context.Context, string, json.RawMessage) (string, error) {
	return "plain-out", nil
}

func TestDispatchRichPlainToolHasNoImage(t *testing.T) {
	r := NewRegistry(plainTool{})
	out, img, err := r.DispatchRich(context.Background(), "plain", ".", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "plain-out" {
		t.Errorf("out = %q", out)
	}
	if img != nil {
		t.Errorf("plain tool must produce no image, got %+v", img)
	}
}

func TestDispatchRichBrowserDeliversScreenshot(t *testing.T) {
	obs := `{"title":"Dashboard","text":"hello","screenshot_b64":"UE5HMTIz"}`
	box := &scriptedBox{result: sandbox.Result{Stdout: obs, ExitCode: 0}}
	r := NewRegistry(BrowserViewTool{Box: box})

	out, img, err := r.DispatchRich(context.Background(), "browser_view", ".", json.RawMessage(`{"url":"http://localhost:3000"}`))
	if err != nil {
		t.Fatal(err)
	}
	if img == nil {
		t.Fatal("browser_view must deliver the screenshot as an image")
	}
	if img.MediaType != "image/png" || img.Base64 != "UE5HMTIz" {
		t.Errorf("image = %+v", img)
	}
	// The base64 payload must NOT be inlined into the fenced text result.
	if want := "UE5HMTIz"; len(out) > 0 && containsSub(out, want) {
		t.Errorf("screenshot base64 must not be inlined in the text result")
	}
}

func TestBrowserViewNoScreenshotNoImage(t *testing.T) {
	obs := `{"title":"T","text":"hi"}` // no screenshot_b64
	box := &scriptedBox{result: sandbox.Result{Stdout: obs, ExitCode: 0}}
	_, img, err := BrowserViewTool{Box: box}.RunWithImage(context.Background(), ".", json.RawMessage(`{"url":"http://x.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if img != nil {
		t.Errorf("no screenshot ⇒ no image, got %+v", img)
	}
}

func TestBrowserViewFailClosedNoImage(t *testing.T) {
	box := &scriptedBox{result: sandbox.Result{Stderr: "nilcore-browser: not found", ExitCode: 127}}
	_, img, err := BrowserViewTool{Box: box}.RunWithImage(context.Background(), ".", json.RawMessage(`{"url":"http://x.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if img != nil {
		t.Errorf("a failed run must yield no image (fail closed), got %+v", img)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
