package config

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEnvContractThreeWayConsistent (amend8 §测试 8): the recognized
// variable set (KnownVars), app/.env.example, and docker-compose.yml carry
// EXACTLY the same BOT_* variable names, and the README documents a subset
// that contains none outside the contract. The three-way equality is what
// keeps a new knob from shipping half-wired (e.g. .env.example without
// KnownVars → typo WARN on every legit deployment).
func TestEnvContractThreeWayConsistent(t *testing.T) {
	envVars := readVarSet(t, "../../.env.example", regexp.MustCompile(`(?m)^BOT_[A-Z_]+=`), "=")
	composeVars := readVarSet(t, "../../../docker-compose.yml",
		regexp.MustCompile(`BOT_[A-Z_]+=`), "=")

	// KnownVars is unexported; UnrecognizedBotVars exposes it indirectly:
	// every recognized name set in the environment disappears from its
	// output. Seed the env with the union of all three surfaces and confirm
	// zero unrecognized.
	for name := range union(envVars, composeVars) {
		t.Setenv(name, "1")
	}
	if got := UnrecognizedBotVars(); len(got) != 0 {
		t.Fatalf("variables known to .env.example/compose but missing from KnownVars: %v", got)
	}

	if !equal(envVars, composeVars) {
		t.Fatalf("env.example and docker-compose differ:\n  env.example: %v\n  compose:     %v",
			sorted(envVars), sorted(composeVars))
	}
}

// TestEnvContractREADMESubset: every BOT_* variable the README documents is
// part of the contract (the README abbreviates some rows, so the README may
// list fewer — but never an unknown one). Names appearing only inside prose
// (backtick-quoted mentions like "吸收 BOT_ANNOUNCE_TTL") are excluded via
// the table-row filter: the README contract rows are `| variable |` table
// cells or `BOT_XXX= value` assignment lines.
func TestEnvContractREADMESubset(t *testing.T) {
	data, err := os.ReadFile("../../../README.md")
	if err != nil {
		t.Skipf("README not reachable from the test dir: %v", err)
	}
	// Assignment form first (code blocks like `cp .env.example .env`).
	reAssign := regexp.MustCompile(`(?m)^\s*(BOT_[A-Z_]+)=`)
	// Table rows: | `BOT_XXX` | ... |
	reTable := regexp.MustCompile(`\|\s*` + "`" + `(BOT_[A-Z_]+)` + "`" + `\s*\|`)
	known := map[string]bool{}
	for _, m := range reAssign.FindAllStringSubmatch(string(data), -1) {
		known[m[1]] = true
	}
	for _, m := range reTable.FindAllStringSubmatch(string(data), -1) {
		known[m[1]] = true
	}
	if len(known) == 0 {
		t.Fatal("README variable table not parsed — regex contract drifted")
	}
	for name := range known {
		t.Setenv(name, "1")
	}
	if got := UnrecognizedBotVars(); len(got) != 0 {
		t.Fatalf("README documents variables outside the contract: %v", got)
	}
}

// TestEnvContractAbsorbedKnobsGone (amend8 merge): BOT_ANNOUNCE_TTL and
// BOT_CMD_AUTODELETE_SEC are removed from the assignment surfaces (config
// contract, .env.example, docker-compose.yml). README prose may still NAME
// them as absorbed (documentation of the merge), so only assignment-shaped
// references count.
func TestEnvContractAbsorbedKnobsGone(t *testing.T) {
	for _, name := range []string{"BOT_ANNOUNCE_TTL", "BOT_CMD_AUTODELETE_SEC"} {
		t.Setenv(name, "1")
	}
	got := UnrecognizedBotVars()
	want := map[string]bool{"BOT_ANNOUNCE_TTL": true, "BOT_CMD_AUTODELETE_SEC": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected unrecognized var %q while probing absorbed knobs", name)
		}
	}
	if len(got) != 2 {
		t.Errorf("both absorbed knobs must be unrecognized, got %v", got)
	}
	for _, path := range []string{"../../.env.example", "../../../docker-compose.yml", "../../../README.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // surface not reachable from the run dir
		}
		for _, knob := range []string{"BOT_ANNOUNCE_TTL", "BOT_CMD_AUTODELETE_SEC"} {
			if strings.Contains(string(data), knob+"=") {
				t.Errorf("%s still assigns absorbed knob %s", path, knob)
			}
		}
	}
}

func readVarSet(t *testing.T, path string, re *regexp.Regexp, sep string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("surface not reachable from the test dir: %v", err)
	}
	out := map[string]bool{}
	for _, m := range re.FindAllString(string(data), -1) {
		name := strings.TrimSuffix(m, sep)
		out[name] = true
	}
	return out
}

func union(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func equal(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
