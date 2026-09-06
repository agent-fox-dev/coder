package toml

import "testing"

// TestArraysOfTablesPreserveOrder is REQ-MCP-CLIENT-07's [[mcp.servers]].
//
// Order is load-bearing: a config that lists servers in a deliberate order and
// gets them back shuffled produces a different tool set on every run once two
// servers expose a name that collides.
func TestArraysOfTablesPreserveOrder(t *testing.T) {
	src := `
[mcp]
enabled = true

[[mcp.servers]]
name = "github"
command = "gh-mcp"

[[mcp.servers]]
name = "db"
command = "db-mcp"
allow_sampling = true
`
	root, _, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	mcp, ok := root.Sub("mcp")
	if !ok {
		t.Fatal("no [mcp] table")
	}
	if v, ok := mcp.Get("enabled"); !ok || !v.Bool {
		t.Fatal("a value beside an array of tables must still be readable")
	}
	servers, ok := mcp.Array("servers")
	if !ok || len(servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(servers))
	}
	first, _ := servers[0].Get("name")
	second, _ := servers[1].Get("name")
	if first.Str != "github" || second.Str != "db" {
		t.Fatalf("order = %q,%q; document order is load-bearing", first.Str, second.Str)
	}
	if v, ok := servers[1].Get("allow_sampling"); !ok || !v.Bool {
		t.Fatal("per-element keys must land in their own element")
	}
	if _, leaked := servers[0].Get("allow_sampling"); leaked {
		t.Fatal("a key from the second element leaked into the first: every later key " +
			"would be filed under the wrong table")
	}
}

func TestAMalformedArrayHeaderIsAHardError(t *testing.T) {
	for _, src := range []string{"[[a]\nx = 1\n", "[[]]\n"} {
		if _, _, err := ParseTOML([]byte(src)); err == nil {
			t.Fatalf("accepted %q", src)
		}
	}
}

// TestASubTableUnderAnArrayElementBelongsToThatElement is the TOML rule that
// [[a]] followed by [a.b] attaches b to the a just declared.
//
// Walking past the array to a root-level [a.b] parses identically and files
// every key in a table nobody reads — which is how a per-server env block ends
// up empty and the subprocess starts with no credential.
func TestASubTableUnderAnArrayElementBelongsToThatElement(t *testing.T) {
	src := `
[[servers]]
name = "one"

[servers.env]
TOKEN = "a"

[[servers]]
name = "two"

[servers.env]
TOKEN = "b"
`
	root, _, err := ParseTOML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	servers, ok := root.Array("servers")
	if !ok || len(servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(servers))
	}
	for i, want := range []string{"a", "b"} {
		env, ok := servers[i].Sub("env")
		if !ok {
			t.Fatalf("element %d has no env sub-table", i)
		}
		v, ok := env.Get("TOKEN")
		if !ok || v.Str != want {
			t.Fatalf("element %d TOKEN = %q, want %q", i, v.Str, want)
		}
	}
	if _, leaked := root.Sub("servers"); leaked {
		t.Fatal("a root-level [servers] table was created alongside the array; the " +
			"sub-table must attach to the array element")
	}
}
