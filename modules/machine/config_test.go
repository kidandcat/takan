package machine

import "testing"

func TestParseConfigDefaults(t *testing.T) {
	c := ParseConfig("")
	if !c.AITasksEnabled {
		t.Fatal("default AI tasks should be enabled")
	}
	if len(c.Runners) < 2 {
		t.Fatalf("expected builtin runners, got %d", len(c.Runners))
	}
	ids := map[string]bool{}
	for _, r := range c.Runners {
		ids[r.ID] = true
		if r.Command == "" {
			t.Fatalf("empty command for %s", r.ID)
		}
	}
	if !ids["claude"] || !ids["grok"] {
		t.Fatalf("missing builtins: %+v", ids)
	}
}

func TestParseConfigPreservesCustom(t *testing.T) {
	raw := `{
	  "ai_tasks_enabled": false,
	  "runners": [
	    {"id":"claude","name":"Claude","enabled":false,"command":"claude -p {{prompt}}","builtin":true},
	    {"id":"mine","name":"My CLI","enabled":true,"command":"mycli {{prompt}}"}
	  ]
	}`
	c := ParseConfig(raw)
	if c.AITasksEnabled {
		t.Fatal("expected disabled")
	}
	r, ok := c.RunnerByID("mine")
	if !ok || !r.Enabled || r.Command != "mycli {{prompt}}" {
		t.Fatalf("custom runner: %+v ok=%v", r, ok)
	}
	en := c.EnabledRunners()
	if len(en) != 1 || en[0].ID != "mine" {
		t.Fatalf("enabled: %+v", en)
	}
}
