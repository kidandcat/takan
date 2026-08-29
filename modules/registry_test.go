package modules

import "testing"

func TestCatalogIncludesTV(t *testing.T) {
	found := false
	for _, c := range Catalog {
		if c.ID == "tv" {
			found = true
			if c.Name != "TV" {
				t.Fatalf("name: %s", c.Name)
			}
			break
		}
	}
	if !found {
		t.Fatal("tv missing from Catalog")
	}
}
