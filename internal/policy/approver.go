package policy

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// ConsoleApprover asks a human to approve an irreversible action on a terminal.
// It is the Phase-1 approver; the Telegram/Slack channels (P1-T05/T06) supply an
// interactive one later, where the gate becomes a chat reply. Default-deny: any
// answer that is not an explicit yes is treated as a refusal.
type ConsoleApprover struct {
	In  io.Reader
	Out io.Writer
}

// NewConsoleApprover wires an approver to the given input/output (normally
// os.Stdin / os.Stdout).
func NewConsoleApprover(in io.Reader, out io.Writer) *ConsoleApprover {
	return &ConsoleApprover{In: in, Out: out}
}

// Approve prompts for the action and returns true only on an explicit "y"/"yes".
func (c *ConsoleApprover) Approve(action string) bool {
	fmt.Fprintf(c.Out, "\nGATE — this action is irreversible:\n  %s\nApprove? [y/N]: ", action)
	sc := bufio.NewScanner(c.In)
	if !sc.Scan() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(sc.Text())) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// ApproveStructured renders the action's evidence payload (when present) ahead
// of the standard prompt, so the operator decides the irreversible step from the
// diffstat / verify facts instead of one flattened line. The block is DATA under
// review (I7): RenderBlock quote-rails every line and nothing in it is executed
// or re-parsed. With no evidence the output is byte-identical to the legacy
// Approve(Describe()) path (pinned by test), so gates that carry no payload —
// and every pre-existing caller — are unchanged.
func (c *ConsoleApprover) ApproveStructured(a GateAction) bool {
	if a.Evidence != nil {
		fmt.Fprint(c.Out, a.Evidence.RenderBlock())
	}
	return c.Approve(a.Describe())
}
