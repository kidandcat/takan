package tg

import (
	"strings"
	"testing"
)

func TestRewriteTablesForTelegramBasic(t *testing.T) {
	in := strings.TrimSpace(`
Gratis | Plus | Pro
--- | --- | ---
0 € | 5,99 €/mes | 11,99 €/mes
100/mes | 1000/mes | ilimitado
`)
	got := RewriteTablesForTelegram(in)
	if !strings.HasPrefix(got, "```\n") || !strings.HasSuffix(got, "\n```") {
		t.Fatalf("expected a fenced block, got:\n%s", got)
	}
	if strings.Contains(got, "|") {
		t.Fatalf("pipes should be gone, got:\n%s", got)
	}
	for _, want := range []string{"Gratis", "Plus", "Pro", "5,99 €/mes", "ilimitado"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	lines := strings.Split(got, "\n")
	// fence, header, sep, two data rows, fence
	if len(lines) != 6 {
		t.Fatalf("expected 6 lines, got %d:\n%s", len(lines), got)
	}
}

func TestRewriteTablesForTelegramPiped(t *testing.T) {
	in := `| Plan | Precio |
| --- | ---: |
| **Gratis** | 0 € |
| Plus | 5,99 € |`
	got := RewriteTablesForTelegram(in)
	if strings.Contains(got, "**") {
		t.Fatalf("emphasis should be stripped, got:\n%s", got)
	}
	if !strings.Contains(got, "Gratis") {
		t.Fatalf("header cell missing:\n%s", got)
	}
}

func TestRewriteTablesForTelegramLeavesProseAndFences(t *testing.T) {
	in := "Hola.\n\n```\n| not | a | table |\n```\n\nSigue."
	if got := RewriteTablesForTelegram(in); got != in {
		t.Fatalf("fenced pipes should be left alone, got:\n%s", got)
	}
	plain := "usa A | B en la receta"
	if got := RewriteTablesForTelegram(plain); got != plain {
		t.Fatalf("prose with a pipe should be left alone, got:\n%s", got)
	}
}

func TestRewriteTablesForTelegramMixed(t *testing.T) {
	in := "Antes\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\nDespués"
	got := RewriteTablesForTelegram(in)
	if !strings.HasPrefix(got, "Antes\n\n```\n") {
		t.Fatalf("prose before the table was lost:\n%s", got)
	}
	if !strings.HasSuffix(got, "```\n\nDespués") {
		t.Fatalf("prose after the table was lost:\n%s", got)
	}
}

func TestRewriteTablesIdempotent(t *testing.T) {
	in := "| A | B |\n| --- | --- |\n| 1 | 2 |"
	once := RewriteTablesForTelegram(in)
	twice := RewriteTablesForTelegram(once)
	if once != twice {
		t.Fatalf("rewrite is not idempotent\n once:\n%s\n twice:\n%s", once, twice)
	}
}
