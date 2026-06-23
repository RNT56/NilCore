package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// This file is CU-MAC-T04: input via cliclick (the zero-custom-native MVP path —
// cliclick wraps CGEvent). Pure arg builders are unit-tested (mirroring the Linux
// xdotool builders); runCliclick is the live seam (CI/host-only). cliclick takes
// global POINTS (the coords.go conversion already produced points).

// cliclickClick builds the cliclick command for a left click at point (x,y).
func cliclickClick(x, y int) []string {
	return []string{"c:" + strconv.Itoa(x) + "," + strconv.Itoa(y)}
}

// cliclickType builds the command to type literal text at the current focus.
func cliclickType(text string) []string {
	return []string{"t:" + text}
}

// cliclickKey builds the command(s) for a key chord like "cmd+s" or "Return".
// cliclick has no single "key chord" verb, so a chord becomes hold-modifiers →
// key/text → release-modifiers; a lone special key is kp:<name>; a lone character is
// typed. Unknown modifiers are dropped (best-effort, never a wrong action).
func cliclickKey(chord string) []string {
	parts := strings.Split(chord, "+")
	if len(parts) == 1 {
		if name, ok := specialKey(parts[0]); ok {
			return []string{"kp:" + name}
		}
		return []string{"t:" + parts[0]}
	}
	mods := mapMods(parts[:len(parts)-1])
	last := parts[len(parts)-1]
	if len(mods) == 0 { // all modifiers unknown → treat the final token alone
		if name, ok := specialKey(last); ok {
			return []string{"kp:" + name}
		}
		return []string{"t:" + last}
	}
	modList := strings.Join(mods, ",")
	cmds := []string{"kd:" + modList}
	if name, ok := specialKey(last); ok {
		cmds = append(cmds, "kp:"+name)
	} else {
		cmds = append(cmds, "t:"+last)
	}
	return append(cmds, "ku:"+modList)
}

// cliclickScroll approximates scroll with page keys (cliclick has no wheel verb;
// the production signed helper does a real CGEvent scroll). amount → repeat count.
func cliclickScroll(dir string, amount int) []string {
	key := "page-down"
	if strings.ToLower(dir) == "up" {
		key = "page-up"
	}
	if amount < 1 {
		amount = 1
	}
	out := make([]string, 0, amount)
	for i := 0; i < amount; i++ {
		out = append(out, "kp:"+key)
	}
	return out
}

// specialKey maps a DOM/X-style key name to cliclick's key name.
func specialKey(k string) (string, bool) {
	switch strings.ToLower(k) {
	case "return", "enter":
		return "return", true
	case "escape", "esc":
		return "esc", true
	case "tab":
		return "tab", true
	case "space":
		return "space", true
	case "backspace", "delete":
		return "delete", true
	case "up":
		return "arrow-up", true
	case "down":
		return "arrow-down", true
	case "left":
		return "arrow-left", true
	case "right":
		return "arrow-right", true
	case "page-up", "pageup", "prior":
		return "page-up", true
	case "page-down", "pagedown", "next":
		return "page-down", true
	case "home":
		return "home", true
	case "end":
		return "end", true
	}
	return "", false
}

// mapMods maps modifier names to cliclick's set (cmd/ctrl/alt/shift/fn).
func mapMods(mods []string) []string {
	var out []string
	for _, m := range mods {
		switch strings.ToLower(m) {
		case "cmd", "command", "meta", "super":
			out = append(out, "cmd")
		case "ctrl", "control":
			out = append(out, "ctrl")
		case "alt", "option", "opt":
			out = append(out, "alt")
		case "shift":
			out = append(out, "shift")
		case "fn":
			out = append(out, "fn")
		}
	}
	return out
}

// runCliclick is the live seam (CI/host-only). A var so tests substitute a recorder.
// A missing cliclick (not installed / no Accessibility grant) surfaces as the error.
var runCliclick = func(ctx context.Context, args []string) error {
	return exec.CommandContext(ctx, "cliclick", args...).Run()
}
