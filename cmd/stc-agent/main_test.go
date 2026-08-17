package main

import "testing"

func TestComponentsEmpty(t *testing.T) {
	if got := components(); len(got) != 0 {
		t.Fatalf("M0 scaffold: expected no components, got %d", len(got))
	}
}
