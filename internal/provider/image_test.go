package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"nilcore/internal/model"
)

// TestAnthropicImageBlockWireShape proves the near-identity Anthropic marshal
// renders an image block in Anthropic's exact content-block shape with no special
// case (P9-T01). The Anthropic request embeds []model.Message directly, so
// marshaling the message is the wire shape.
func TestAnthropicImageBlockWireShape(t *testing.T) {
	msg := model.Message{Role: "user", Content: []model.Block{model.ImageBlock("image/png", "aGVsbG8=")}}
	got, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]}`
	if string(got) != want {
		t.Fatalf("anthropic image shape:\n got %s\nwant %s", got, want)
	}
}

// TestImageBlockOmittedForNonImage proves the additive Source field never appears
// on a non-image block — the byte-identical guarantee at the type level.
func TestImageBlockOmittedForNonImage(t *testing.T) {
	for _, b := range []model.Block{
		{Type: "text", Text: "hi"},
		{Type: "tool_use", ID: "t1", Name: "read", Input: json.RawMessage(`{}`)},
		{Type: "tool_result", ToolUseID: "t1", Content: "ok"},
	} {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal %s: %v", b.Type, err)
		}
		if strings.Contains(string(raw), "source") {
			t.Errorf("%s block leaked a source field: %s", b.Type, raw)
		}
	}
}

// TestOpenAIImageToContentParts proves toOpenAIMessages translates an image block
// into an image_url content part with a base64 data URI, alongside leading text.
func TestOpenAIImageToContentParts(t *testing.T) {
	msgs := []model.Message{{Role: "user", Content: []model.Block{
		{Type: "text", Text: "what is this?"},
		model.ImageBlock("image/jpeg", "QUJD"),
	}}}
	out := toOpenAIMessages("", msgs)
	if len(out) != 1 {
		t.Fatalf("want 1 message, got %d", len(out))
	}
	parts, ok := out[0].Content.([]oaiContentPart)
	if !ok {
		t.Fatalf("content is %T, want []oaiContentPart", out[0].Content)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[1].Type != "image_url" {
		t.Fatalf("parts = %+v; want [text, image_url]", parts)
	}
	if parts[0].Text != "what is this?" {
		t.Errorf("text part = %q", parts[0].Text)
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/jpeg;base64,QUJD" {
		t.Errorf("image_url = %+v; want data:image/jpeg;base64,QUJD", parts[1].ImageURL)
	}
}

// TestOpenAIMessagesByteIdenticalNoImage proves the multimodal change did not
// alter the wire output for an image-free conversation: text content stays a
// plain string and an assistant message that is only tool_calls omits "content".
func TestOpenAIMessagesByteIdenticalNoImage(t *testing.T) {
	msgs := []model.Message{
		{Role: "user", Content: []model.Block{{Type: "text", Text: "hello"}}},
		{Role: "assistant", Content: []model.Block{{Type: "tool_use", ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"x"}`)}}},
		{Role: "user", Content: []model.Block{{Type: "tool_result", ToolUseID: "c1", Content: "file body"}}},
	}
	raw, err := json.Marshal(toOpenAIMessages("you are a worker", msgs))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[` +
		`{"role":"system","content":"you are a worker"},` +
		`{"role":"user","content":"hello"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"x\"}"}}]},` +
		`{"role":"tool","content":"file body","tool_call_id":"c1"}` +
		`]`
	if string(raw) != want {
		t.Fatalf("no-image serialization drifted:\n got %s\nwant %s", raw, want)
	}
}
