package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kidandcat/takan/modules/mercadona/client"
	"github.com/kidandcat/takan/modules/mercadona/store"
)

func TestPreferredLearnAndFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := New(db, store.LocalAccountID)
	ctx := context.Background()

	if err := svc.upsertPreferred(ctx, "111", "Leche entera"); err != nil {
		t.Fatal(err)
	}
	if err := svc.upsertPreferred(ctx, "222", "Leche desnatada"); err != nil {
		t.Fatal(err)
	}
	// Bump use_count for 111.
	if err := svc.upsertPreferred(ctx, "111", "Leche entera 1L"); err != nil {
		t.Fatal(err)
	}

	prefs, err := svc.ListPreferred(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefs) != 2 {
		t.Fatalf("want 2 preferred, got %d", len(prefs))
	}
	if prefs[0].ProductID != "111" || prefs[0].UseCount != 2 {
		t.Fatalf("expected 111 first with use_count=2, got %+v", prefs[0])
	}
	if prefs[0].ProductName != "Leche entera 1L" {
		t.Fatalf("name not updated: %q", prefs[0].ProductName)
	}

	hits := []client.Product{
		{ID: "999", DisplayName: "Agua"},
		{ID: "111", DisplayName: "Leche entera 1L"},
		{ID: "888", DisplayName: "Pan"},
	}
	prefHits, err := svc.filterPreferred(ctx, hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefHits) != 1 || prefHits[0].ID != "111" {
		t.Fatalf("want single preferred 111, got %+v", prefHits)
	}

	// Two preferred among hits → still ambiguous at caller level.
	hits2 := []client.Product{
		{ID: "111", DisplayName: "Leche entera"},
		{ID: "222", DisplayName: "Leche desnatada"},
	}
	prefHits2, err := svc.filterPreferred(ctx, hits2)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefHits2) != 2 {
		t.Fatalf("want 2 preferred hits, got %d", len(prefHits2))
	}

	if err := svc.DeletePreferred(ctx, prefs[0].ID); err != nil {
		t.Fatal(err)
	}
	left, err := svc.ListPreferred(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ProductID != "222" {
		t.Fatalf("want only 222 left, got %+v", left)
	}
}

func TestUpsertAliasAndPreferredOnAddByIDPath(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := New(db, store.LocalAccountID)
	ctx := context.Background()

	if err := svc.upsertPreferred(ctx, "10379", "Huevos L"); err != nil {
		t.Fatal(err)
	}
	if err := svc.upsertAlias(ctx, "huevos", "10379", "Huevos L"); err != nil {
		t.Fatal(err)
	}

	aliases, err := svc.ListAliases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 1 || aliases[0].Alias != "huevos" || aliases[0].ProductID != "10379" {
		t.Fatalf("unexpected aliases: %+v", aliases)
	}
}
