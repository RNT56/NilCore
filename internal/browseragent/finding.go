package browseragent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"nilcore/internal/artifact"
)

// FindingTool (record_finding) is the Phase-14 extract→artifact bridge. When the
// browse agent extracts a datum, it calls record_finding{field, value, url?}; the
// tool appends a typed artifact.Claim — verifier-id ui.value_present, SourceURL the
// current page (or an explicit url), Status Unverified — to the worktree artifact
// at .nilcore/artifacts/<id>.json. The browse run's ArtifactVerifier then RE-DERIVES
// each claim in-box (re-navigate to the source, confirm the value is present) and
// overwrites the status, so a finding ships GREEN only because the harness confirmed
// it — never because the agent reported it (I2). The recorded value is the model's
// untrusted assertion; it becomes trusted only after the verifier re-derives it (I7).
type FindingTool struct {
	Root       string  // worktree root the artifact is written under (= box.Workdir())
	ArtifactID string  // the artifact id (a single safe path component)
	Title      string  // human title for the artifact
	Sess       Session // for the current page URL when a finding omits one

	mu sync.Mutex
	n  int // claim counter for stable ids
}

func (*FindingTool) Name() string { return "record_finding" }
func (*FindingTool) Description() string {
	return "Record one extracted datum as a verifiable finding. The harness will RE-DERIVE it " +
		"independently (re-open the source and confirm the value is present) — so only record what is " +
		"actually on the page at the given url. Args: field (a short label), value (the exact datum, as " +
		"it appears on the page), url (optional — defaults to the current page). Recording a value that is " +
		"not really on the source page will fail verification and the run will not be done."
}
func (*FindingTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"field":{"type":"string","description":"a short semantic label, e.g. \"latest_version\""},` +
		`"value":{"type":"string","description":"the exact extracted datum as it appears on the page"},` +
		`"url":{"type":"string","description":"the source page (defaults to the current page)"}` +
		`},"required":["field","value"]}`)
}

func (f *FindingTool) Run(ctx context.Context, _ string, input json.RawMessage) (string, error) {
	var in struct {
		Field string `json:"field"`
		Value string `json:"value"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", fmt.Errorf("bad record_finding input: %w", err)
	}
	in.Field = strings.TrimSpace(in.Field)
	in.Value = strings.TrimSpace(in.Value)
	if in.Field == "" || in.Value == "" {
		return "", errors.New("record_finding: field and value are both required")
	}
	src := strings.TrimSpace(in.URL)
	if src == "" && f.Sess != nil {
		src = f.Sess.Latest().URL // default to the current page
	}
	if src == "" {
		return "", errors.New("record_finding: no url given and no current page — navigate first or pass url")
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Read-or-create the artifact, append the claim, write it back atomically. The
	// artifact lives in the worktree the verifier scans (.nilcore/artifacts/<id>.json).
	a, err := artifact.Read(f.Root, f.ArtifactID)
	if err != nil {
		a = &artifact.Artifact{
			SchemaVersion: artifact.SchemaVersion,
			ID:            f.ArtifactID,
			Kind:          artifact.KindDossier,
			Title:         f.Title,
		}
	}
	f.n++
	claim := artifact.Claim{
		ID:    fmt.Sprintf("%s-%d", in.Field, f.n),
		Field: in.Field,
		Evidence: artifact.Evidence{
			Value:            in.Value,
			SourceURL:        src,
			ExtractionMethod: "browse",
			Verifier:         "ui.value_present",
			Status:           artifact.StatusUnverified, // the verifier sets the real status (I2)
		},
	}
	a.Claims = append(a.Claims, claim)
	if err := artifact.Write(f.Root, a); err != nil {
		return "", fmt.Errorf("record_finding: writing artifact: %w", err)
	}
	return fmt.Sprintf("recorded finding %q = %q (source %s); it will be verified independently before the run is done.", in.Field, in.Value, src), nil
}
