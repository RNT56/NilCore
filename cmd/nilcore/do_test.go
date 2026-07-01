package main

import (
	"reflect"
	"testing"

	"nilcore/internal/router"
)

func TestResolvePreset(t *testing.T) {
	t.Run("routes by goal when -as is empty", func(t *testing.T) {
		p, by, err := resolvePreset("", "build a new service from scratch")
		if err != nil || p != router.Build || by != "heuristic" {
			t.Fatalf("resolvePreset = (%q,%q,%v), want (build,heuristic,nil)", p, by, err)
		}
	})
	t.Run("ordinary task routes to run", func(t *testing.T) {
		p, by, err := resolvePreset("", "fix the panic in auth.go")
		if err != nil || p != router.Run || by != "heuristic" {
			t.Fatalf("resolvePreset = (%q,%q,%v), want (run,heuristic,nil)", p, by, err)
		}
	})
	t.Run("-as forces the preset over the heuristic", func(t *testing.T) {
		// Goal would route to run, but -as build forces build.
		p, by, err := resolvePreset("build", "fix a typo")
		if err != nil || p != router.Build || by != "forced" {
			t.Fatalf("resolvePreset = (%q,%q,%v), want (build,forced,nil)", p, by, err)
		}
	})
	t.Run("-as is case/space tolerant", func(t *testing.T) {
		p, _, err := resolvePreset("  SWARM ", "anything")
		if err != nil || p != router.Swarm {
			t.Fatalf("resolvePreset(-as ' SWARM ') = (%q,%v), want (swarm,nil)", p, err)
		}
	})
	t.Run("-as decompose is a valid forced preset", func(t *testing.T) {
		p, by, err := resolvePreset("decompose", "a and b and c")
		if err != nil || p != router.Decompose || by != "forced" {
			t.Fatalf("resolvePreset(-as decompose) = (%q,%q,%v), want (decompose,forced,nil)", p, by, err)
		}
	})
	t.Run("-as with an unknown preset fails closed", func(t *testing.T) {
		if _, _, err := resolvePreset("teleport", "anything"); err == nil {
			t.Fatal("resolvePreset(-as teleport) = nil error, want an error")
		}
	})
}

func TestPresetArgs(t *testing.T) {
	cases := []struct {
		name        string
		preset      router.Preset
		goal, dir   string
		swarmPreset string
		want        []string
	}{
		{"run carries goal+dir", router.Run, "do a thing", "./repo", "", []string{"-goal", "do a thing", "-dir", "./repo"}},
		{"build carries goal+dir", router.Build, "make it", ".", "", []string{"-goal", "make it", "-dir", "."}},
		{"swarm forwards -dir too", router.Swarm, "audit all", "./other", "", []string{"-goal", "audit all", "-dir", "./other"}},
		{"swarm adds -preset when given", router.Swarm, "audit all", ".", "research", []string{"-goal", "audit all", "-dir", ".", "-preset", "research"}},
		{"swarm ignores a blank -preset", router.Swarm, "audit all", ".", "   ", []string{"-goal", "audit all", "-dir", "."}},
		{"decompose carries goal+dir", router.Decompose, "a and b", "./repo", "", []string{"-goal", "a and b", "-dir", "./repo"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := presetArgs(c.preset, c.goal, c.dir, c.swarmPreset)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("presetArgs = %v, want %v", got, c.want)
			}
		})
	}
}
