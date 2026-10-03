package config

import "testing"

func TestToolBindingsSeparateSelectionApplicationAndCopy(t *testing.T) {
	a := LocalAccount{ID: "one", Kind: "codex", Name: "first", BaseURL: "https://api.test", Key: "private-one", Model: "model", AuthMode: "bearer"}
	b := a
	b.ID, b.Key = "two", "private-two"
	c := Config{LocalAccounts: []LocalAccount{a, b}, ActiveCodexAccount: a.ID}
	c.RecordAppliedAPI("codex-desktop", a, "original-provider")
	c.ToolAPISelections["codex"] = b.ID
	selected, _ := c.SelectedToolAccount("codex")
	applied, valid := c.AppliedToolAccount("codex")
	if selected.ID != b.ID || !valid || applied.ID != a.ID {
		t.Fatal("selection applied credentials or lost shared family")
	}
	clone := c.Clone()
	clone.ToolAPISelections["codex"] = ""
	delete(clone.ToolAPIApplied, "codex")
	if c.ToolAPISelections["codex"] != b.ID || c.ToolAPIApplied["codex"].Provider != "original-provider" {
		t.Fatal("clone aliases tool maps")
	}
	if _, ok := clone.SelectedToolAccount("codex"); ok {
		t.Fatal("explicit deselection fell back to default")
	}
	c.LocalAccounts[0].Name = "renamed"
	if _, ok := c.AppliedToolAccount("codex"); !ok {
		t.Fatal("rename unnecessarily invalidated API")
	}
	c.LocalAccounts[0].Key = "edited-key"
	if _, ok := c.AppliedToolAccount("codex"); ok {
		t.Fatal("edited API silently became applied")
	}
}
